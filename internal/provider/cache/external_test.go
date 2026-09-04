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
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

const testConnectionName = "shared-redis"

// newExternal is a `redis` connection to one server, with the platform's own
// cluster behind it because that is where the allocations are recorded.
func newExternal(t *testing.T, config string) (*External, client.Client) {
	t.Helper()
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: testConnectionName, Namespace: "kitchen-system"},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: ProviderRedis},
	}
	if config != "" {
		conn.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(conn).
		WithStatusSubresource(conn).
		Build()
	return externalOver(t, c, conn), c
}

// externalOver is a second provisioner over the same cluster: the next
// reconcile, which builds its provisioner again from the same Connection.
func externalOver(t *testing.T, c client.Client, conn *kitchenv1alpha1.Connection) *External {
	t.Helper()
	provisioner, err := NewExternal(Options{
		Connection: conn,
		URL:        "rediss://:hunter2@redis.example.com:6379",
		Cluster:    c,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner
}

func connection(t *testing.T, c client.Client) *kitchenv1alpha1.Connection {
	t.Helper()
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: "kitchen-system", Name: testConnectionName}
	if err := c.Get(context.Background(), key, conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

func mustProvision(t *testing.T, e *External, res naming.Resource) Instance {
	t.Helper()
	instance, err := e.Provision(context.Background(), res)
	if err != nil {
		t.Fatalf("provisioning %s/%s: %v", res.Project, res.Claim, err)
	}
	return instance
}

// The finding this exists for: every claim through one connection used to be
// handed `<url>/0`, so one project's application could read, overwrite and
// FLUSHALL another's. Two claims now hold two databases, and the binding
// says which in the URL and beside it, because a client given the host, the
// port and the password alone connects to database 0.
func TestTwoClaimsThroughOneConnectionGetDifferentDatabases(t *testing.T) {
	provisioner, _ := newExternal(t, "")

	mine := mustProvision(t, provisioner, shopCache)
	theirs := mustProvision(t, provisioner, naming.Resource{Project: "warehouse", Claim: "cache"})

	if mine.Binding.Database == theirs.Binding.Database {
		t.Fatalf("two claims were handed database %s between them", mine.Binding.Database)
	}
	for _, instance := range []Instance{mine, theirs} {
		if instance.Binding.Database == "0" {
			t.Errorf("%s was put in database 0, which belongs to bindings made before allocation", instance.Name)
		}
		if !strings.HasSuffix(instance.Binding.URL, "/"+instance.Binding.Database) {
			t.Errorf("the url must select the database it was allocated: %s", instance.Binding.URL)
		}
		if got := string(instance.Binding.Data()[BindingKeyDatabase]); got != instance.Binding.Database {
			t.Errorf("the binding secret must carry the database: %q", got)
		}
	}
}

// The allocation is a record, not a hash of the name: it is read back on
// every reconcile, including one that builds the provisioner again.
func TestAnAllocationSurvivesTheNextReconcile(t *testing.T) {
	provisioner, c := newExternal(t, "")
	first := mustProvision(t, provisioner, shopCache)

	// The same reconcile again, and then the next one — the claim now
	// carrying the name it was bound under, as the controller records it.
	again := mustProvision(t, provisioner, shopCache)
	bound := naming.Resource{Project: shopCache.Project, Claim: shopCache.Claim, Name: first.Name}
	next := mustProvision(t, externalOver(t, c, connection(t, c)), bound)

	if again.Binding.Database != first.Binding.Database || next.Binding.Database != first.Binding.Database {
		t.Fatalf("the database moved between reconciles: %s, %s, %s",
			first.Binding.Database, again.Binding.Database, next.Binding.Database)
	}

	// And it is on the connection, where an operator can read it and every
	// other claim through this connection has to look.
	conn := connection(t, c)
	if conn.Status.Cache == nil || len(conn.Status.Cache.Databases) != 1 {
		t.Fatalf("the connection records %+v", conn.Status.Cache)
	}
	held := conn.Status.Cache.Databases[0]
	if held.Holder != first.Name {
		t.Errorf("the record must name the holder, got %q", held.Holder)
	}
}

// A claim bound before the platform allocated databases keeps database 0.
// Moving it would hand the application an empty keyspace and leave its data
// where nothing reads it — and nothing new is ever put in there with it.
func TestAClaimBoundBeforeAllocationKeepsDatabaseZero(t *testing.T) {
	provisioner, c := newExternal(t, "")

	// Bound under #340's naming (the name recorded) and bound before it
	// (nothing recorded, so the unqualified name): both are bindings this
	// provider made when every claim got `<url>/0`.
	recorded := naming.Resource{Project: "shop", Claim: "cache", Name: "kitchen-shop-cache"}
	older := naming.Resource{Project: "warehouse", Claim: "cache", Unqualified: true}
	for _, res := range []naming.Resource{recorded, older} {
		instance := mustProvision(t, provisioner, res)
		if instance.Binding.Database != "0" {
			t.Fatalf("a bound claim moved to database %s", instance.Binding.Database)
		}
	}

	// A claim binding now does not join them there.
	fresh := mustProvision(t, provisioner, shopJobs)
	if fresh.Binding.Database == "0" {
		t.Fatal("a new claim was put in database 0 with the claims that were already there")
	}
	conn := connection(t, c)
	if len(conn.Status.Cache.Databases) != 3 {
		t.Fatalf("every holding is recorded, including the ones on database 0: %+v", conn.Status.Cache.Databases)
	}
}

// A server with nothing left refuses the claim, naming the constraint. It is
// the answer that used to be "share database 0 with whoever is already
// there", which is the whole bug.
func TestARunOutOfDatabasesIsARefusal(t *testing.T) {
	provisioner, _ := newExternal(t, `{"databases": 3}`)

	first := mustProvision(t, provisioner, shopCache)
	second := mustProvision(t, provisioner, shopJobs)
	if first.Binding.Database == second.Binding.Database {
		t.Fatalf("both claims were handed database %s", first.Binding.Database)
	}

	_, err := provisioner.Provision(context.Background(), naming.Resource{Project: "warehouse", Claim: "cache"})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("want ErrUnsatisfiable when the server has no database left, got %v", err)
	}
	for _, says := range []string{"databases", testConnectionName, ProviderValkey} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
}

// A connection that says its server serves one database has none to give:
// database 0 is not allocated to anybody.
func TestAServerWithOnlyDatabaseZeroRefusesEveryClaim(t *testing.T) {
	provisioner, _ := newExternal(t, `{"databases": 1}`)
	_, err := provisioner.Provision(context.Background(), shopCache)
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("want ErrUnsatisfiable, got %v", err)
	}
	if !strings.Contains(err.Error(), "databases") {
		t.Errorf("the refusal does not name the setting to change: %v", err)
	}
}

// Previews are allocated from the same pool as the claims, so no two live
// previews can collide — which the hash they used to be given could — and a
// closed preview gives its database back rather than holding one of sixteen
// forever.
func TestPreviewsAreAllocatedAndGiveTheirDatabaseBack(t *testing.T) {
	provisioner, c := newExternal(t, "")
	ctx := context.Background()
	claim := mustProvision(t, provisioner, shopCache)

	first, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	second, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-9")
	if err != nil {
		t.Fatal(err)
	}
	databases := map[string]bool{
		claim.Binding.Database:  true,
		first.Binding.Database:  true,
		second.Binding.Database: true,
	}
	if len(databases) != 3 {
		t.Fatalf("the claim and its two previews share a database: %v", databases)
	}

	// A preview reconciled again keeps what it has.
	again, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if again.Binding.Database != first.Binding.Database {
		t.Fatalf("the preview's database moved from %s to %s", first.Binding.Database, again.Binding.Database)
	}

	// Closing it hands the database back, and the row stays because the
	// platform cannot empty a server it does not run.
	if err := provisioner.DeleteBranch(ctx, claim.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	conn := connection(t, c)
	for _, held := range conn.Status.Cache.Databases {
		if held.Holder == first.ID {
			t.Fatalf("the closed preview still holds database %d", held.Database)
		}
	}
	if len(conn.Status.Cache.Databases) != 3 {
		t.Fatalf("a database that has been used is remembered as used: %+v", conn.Status.Cache.Databases)
	}

	// The next preview takes a database nothing has ever held before it
	// takes the one that was given back.
	third, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-11")
	if err != nil {
		t.Fatal(err)
	}
	if third.Binding.Database == first.Binding.Database {
		t.Errorf("a preview inherited the database a closed preview left behind while %d were untouched",
			DefaultExternalDatabases-4)
	}
}

// Everything a server has is handed out before anything is handed out twice,
// and a database given back is reused rather than lost.
func TestAReleasedDatabaseIsReusedOnlyWhenNothingElseIsLeft(t *testing.T) {
	provisioner, _ := newExternal(t, `{"databases": 3}`)
	ctx := context.Background()

	claim := mustProvision(t, provisioner, shopCache)
	preview, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := provisioner.DeleteBranch(ctx, claim.ID, preview.ID); err != nil {
		t.Fatal(err)
	}

	next, err := provisioner.CreateBranch(ctx, claim.ID, "shop-pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if next.Binding.Database != preview.Binding.Database {
		t.Fatalf("the only free database is the one that was given back: want %s, got %s",
			preview.Binding.Database, next.Binding.Database)
	}
}

// Deleting a claim under deletionPolicy Delete gives its database back too;
// under Retain nothing calls this, and the record keeps the database for the
// claim that rebinds to it.
func TestDeprovisionGivesTheDatabaseBack(t *testing.T) {
	provisioner, c := newExternal(t, "")
	claim := mustProvision(t, provisioner, shopCache)

	if err := provisioner.Deprovision(context.Background(), claim.ID); err != nil {
		t.Fatal(err)
	}
	for _, held := range connection(t, c).Status.Cache.Databases {
		if held.Holder != "" {
			t.Fatalf("database %d is still held by %q", held.Database, held.Holder)
		}
	}
}

// The provider records its allocations on the Connection, so it is refused
// rather than built when it has nowhere to write them: a provisioner that
// cannot record an allocation would fall back to sharing database 0, which
// is what this closes.
func TestExternalNeedsSomewhereToRecordAllocations(t *testing.T) {
	_, err := NewExternal(Options{
		Connection: &kitchenv1alpha1.Connection{Spec: kitchenv1alpha1.ConnectionSpec{Provider: ProviderRedis}},
		URL:        "redis://redis.example.com:6379",
	})
	if err == nil {
		t.Fatal("a redis provisioner without the platform's cluster must be refused")
	}
}

// Two claims allocating at the same moment must not both be told database 1.
// The record is written with the version it was read at, so the second
// allocation is refused, reads what the first wrote, and takes the next one.
func TestAnAllocationRacedByAnotherTakesTheNextDatabase(t *testing.T) {
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: testConnectionName, Namespace: "kitchen-system"},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: ProviderRedis},
	}
	raced := false
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(conn).
		WithStatusSubresource(conn).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				ctx context.Context,
				inner client.Client,
				subResource string,
				obj client.Object,
				opts ...client.SubResourceUpdateOption,
			) error {
				if !raced {
					// Somebody else's claim gets database 1 in between this
					// allocation's read and its write.
					raced = true
					other := &kitchenv1alpha1.Connection{}
					key := types.NamespacedName{Namespace: "kitchen-system", Name: testConnectionName}
					if err := inner.Get(ctx, key, other); err != nil {
						return err
					}
					other.Status.Cache = &kitchenv1alpha1.CacheConnectionStatus{
						Databases: []kitchenv1alpha1.CacheDatabase{
							{Database: FirstAllocatableDatabase, Holder: "kitchen-warehouse-cache"},
						},
					}
					if err := inner.Status().Update(ctx, other); err != nil {
						return err
					}
				}
				return inner.SubResource(subResource).Update(ctx, obj, opts...)
			},
		}).
		Build()

	instance := mustProvision(t, externalOver(t, c, conn), shopCache)
	if !raced {
		t.Fatal("the race this test is about did not happen")
	}
	if instance.Binding.Database == strconv.Itoa(FirstAllocatableDatabase) {
		t.Fatalf("both claims were handed database %s", instance.Binding.Database)
	}
	holders := map[string]int{}
	for _, held := range connection(t, c).Status.Cache.Databases {
		holders[held.Holder] = held.Database
	}
	if len(holders) != 2 {
		t.Fatalf("the record must keep both allocations: %v", holders)
	}
}

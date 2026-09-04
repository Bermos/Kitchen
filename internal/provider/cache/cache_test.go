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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// The two claims these tests provision for, both of project "shop": the
// instance's name is kitchen-<project>-<claim>.
var (
	shopCache = naming.Resource{Project: "shop", Claim: "cache"}
	shopJobs  = naming.Resource{Project: "shop", Claim: "jobs"}
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newValkey(t *testing.T) (*Valkey, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	provisioner, err := NewValkey(Options{
		Connection: &kitchenv1alpha1.Connection{Spec: kitchenv1alpha1.ConnectionSpec{Provider: ProviderValkey}},
		Cluster:    c,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, c
}

// The difference between the two usages is four arguments, and getting them
// backwards is the incident this contract exists to prevent — so it is
// asserted on the arguments themselves rather than on anything downstream.
func TestUsageDecidesEvictionAndDurability(t *testing.T) {
	for _, testCase := range []struct {
		usage     Usage
		wants     []string
		rejects   []string
		hasVolume bool
	}{
		{
			usage:     UsageCache,
			wants:     []string{"allkeys-lru", "--appendonly", "no"},
			rejects:   []string{"noeviction"},
			hasVolume: false,
		},
		{
			usage:     UsageQueue,
			wants:     []string{"noeviction", "--appendonly", "yes"},
			rejects:   []string{"allkeys-lru"},
			hasVolume: true,
		},
	} {
		t.Run(string(testCase.usage), func(t *testing.T) {
			provisioner, _ := newValkey(t)
			cfg, err := provisioner.resolve(Requirements{Usage: testCase.usage})
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(valkeyArgs(cfg), " ")
			for _, want := range testCase.wants {
				if !strings.Contains(args, want) {
					t.Errorf("a %s must be started with %q: %s", testCase.usage, want, args)
				}
			}
			for _, reject := range testCase.rejects {
				if strings.Contains(args, reject) {
					t.Errorf("a %s must not be started with %q: %s", testCase.usage, reject, args)
				}
			}
			set := provisioner.desiredStatefulSet("inst", cfg)
			if got := len(set.Spec.VolumeClaimTemplates) > 0; got != testCase.hasVolume {
				t.Errorf("a %s wants a volume: %v, got %v", testCase.usage, testCase.hasVolume, got)
			}
		})
	}
}

// A claim naming nothing gets a cache: the safe default, because a cache
// that turns out to be a queue loses work where a queue that turns out to be
// a cache only costs a volume.
func TestNoUsageIsACache(t *testing.T) {
	provisioner, _ := newValkey(t)
	cfg, err := provisioner.resolve(Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.usage != UsageCache {
		t.Fatalf("want cache, got %q", cfg.usage)
	}
}

// Everything the provisioner cannot supply is refused before it has created
// anything, which is what ErrUnsatisfiable is for.
func TestRefusesWhatItCannotSupply(t *testing.T) {
	provisioner, _ := newValkey(t)
	for _, testCase := range []struct {
		name string
		req  Requirements
		says string
	}{
		{"a version nothing publishes", Requirements{Version: "3"}, "can run"},
		{"a usage that is not one", Requirements{Usage: "durable"}, "not one of"},
		{"a memory limit that is not a quantity", Requirements{MaxMemory: "lots"}, "quantity"},
		{"a memory limit of nothing", Requirements{MaxMemory: "0"}, "more than nothing"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := provisioner.resolve(testCase.req)
			if !errors.Is(err, ErrUnsatisfiable) {
				t.Fatalf("want ErrUnsatisfiable, got %v", err)
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Fatalf("the refusal does not say %q: %v", testCase.says, err)
			}
		})
	}
}

// An instance is not bound until it is serving: the claim waits Pending
// rather than handing an application an address nothing answers on.
func TestProvisionWaitsUntilTheInstanceServes(t *testing.T) {
	provisioner, c := newValkey(t)
	ctx := context.Background()

	_, err := provisioner.Provision(ctx, shopCache)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady while it starts, got %v", err)
	}

	// The objects are there, and the password was minted once.
	set := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-shop-cache"}
	if err := c.Get(ctx, key, set); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	password := string(secret.Data[BindingKeyPassword])
	if len(password) < 32 {
		t.Fatalf("the password looks too short to be random: %q", password)
	}

	// Once it is serving, the binding carries the address and that password.
	set.Status.ReadyReplicas = 1
	if err := c.Status().Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	instance, err := provisioner.Provision(ctx, shopCache)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Binding.Password != password {
		t.Error("the binding must carry the password that was minted, not a second one")
	}
	if !strings.Contains(instance.Binding.URL, DefaultCacheNamespace) {
		t.Errorf("the url must address the instance's service: %s", instance.Binding.URL)
	}
	if instance.Provenance != ProvenanceProduction {
		t.Errorf("the claim's own instance is production's: %q", instance.Provenance)
	}
}

// A preview's instance is configured like the one it branches from — a
// queue's preview is a queue — and holds none of its keys.
func TestABranchInheritsTheUsageAndIsEmpty(t *testing.T) {
	provisioner, c := newValkey(t)
	ctx := context.Background()

	if _, err := provisioner.ProvisionWith(ctx, shopJobs, Requirements{Usage: UsageQueue}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	ready(t, c, DefaultCacheNamespace, "kitchen-shop-jobs")

	instance, err := provisioner.ProvisionWith(ctx, shopJobs, Requirements{Usage: UsageQueue})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.CreateBranch(ctx, instance.ID, "pr-7"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady while the branch starts, got %v", err)
	}
	child := branchName(instance.ID, "pr-7")
	ready(t, c, DefaultCacheNamespace, child)

	branch, err := provisioner.CreateBranch(ctx, instance.ID, "pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if branch.Provenance != ProvenanceSynthetic {
		t.Errorf("a preview's instance is empty and says so: %q", branch.Provenance)
	}
	set := &appsv1.StatefulSet{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: DefaultCacheNamespace, Name: child}, set); err != nil {
		t.Fatal(err)
	}
	if got := set.Annotations[usageAnnotation]; got != string(UsageQueue) {
		t.Errorf("a queue's preview must be a queue, got %q", got)
	}
	if len(set.Spec.VolumeClaimTemplates) == 0 {
		t.Error("a preview queue keeps its jobs on a volume too")
	}
}

// Deprovisioning takes the volume with it. A StatefulSet's PVCs are not
// collected with the StatefulSet, so a queue deleted under Delete would
// otherwise leave its jobs and their cost behind.
func TestDeprovisionRemovesTheVolumeToo(t *testing.T) {
	provisioner, c := newValkey(t)
	ctx := context.Background()

	if _, err := provisioner.ProvisionWith(ctx, shopJobs, Requirements{Usage: UsageQueue}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	// The volume a StatefulSet's claim template leaves behind, as the API
	// server would have created it.
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      "data-kitchen-shop-jobs-0",
		Namespace: DefaultCacheNamespace,
		Labels:    map[string]string{instanceLabel: "kitchen-shop-jobs"},
	}, Spec: corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
		},
	}}
	if err := c.Create(ctx, pvc); err != nil {
		t.Fatal(err)
	}

	if err := provisioner.Deprovision(ctx, DefaultCacheNamespace+"/kitchen-shop-jobs"); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{
		&appsv1.StatefulSet{}, &corev1.Service{}, &corev1.Secret{},
	} {
		key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-shop-jobs"}
		if err := c.Get(ctx, key, object); err == nil {
			t.Errorf("%T outlived the instance", object)
		}
	}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: DefaultCacheNamespace, Name: "data-kitchen-shop-jobs-0",
	}, &corev1.PersistentVolumeClaim{}); err == nil {
		t.Error("the volume outlived the instance it belonged to")
	}
}

// Two claims whose names differ only past the budget get two instances, not
// one shared by accident.
func TestLongNamesDoNotCollide(t *testing.T) {
	long := strings.Repeat("a", 60)
	first := naming.Resource{Project: "shop", Claim: long + "-one"}.Qualified(maxInstanceName)
	second := naming.Resource{Project: "shop", Claim: long + "-two"}.Qualified(maxInstanceName)
	if first == second {
		t.Fatalf("two claims collided on %q", first)
	}
	for _, name := range []string{first, second} {
		if len(name) > maxInstanceName {
			t.Errorf("%q is over the budget", name)
		}
	}
}

func ready(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	set := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, set); err != nil {
		t.Fatal(err)
	}
	set.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), set); err != nil {
		t.Fatal(err)
	}
}

// An instance is named after the project as well as the claim and records
// the project on itself, so a claim of the same name in another project
// never reaches an instance left behind under deletionPolicy Retain.
func TestAnInstanceCarriesTheProjectThatClaimedIt(t *testing.T) {
	provisioner, c := newValkey(t)
	ctx := context.Background()

	if _, err := provisioner.Provision(ctx, shopCache); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	set := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-shop-cache"}
	if err := c.Get(ctx, key, set); err != nil {
		t.Fatal(err)
	}
	if got := set.Labels[naming.LabelProject]; got != "shop" {
		t.Fatalf("the instance records project %q", got)
	}

	// Another project's claim of the same name gets an instance of its own.
	theirs := naming.Resource{Project: "warehouse", Claim: "cache"}
	if _, err := provisioner.Provision(ctx, theirs); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want an instance of its own, still starting, got %v", err)
	}
	other := &appsv1.StatefulSet{}
	otherKey := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-warehouse-cache"}
	if err := c.Get(ctx, otherKey, other); err != nil {
		t.Fatal(err)
	}
	if got := other.Labels[naming.LabelProject]; got != "warehouse" {
		t.Fatalf("the second instance records project %q", got)
	}
}

// An instance from before the project was in the name is not adopted by
// whoever asks for it first.
func TestAnInstanceNamedBeforeTheProjectIsRefusedUntilItIsHandedOver(t *testing.T) {
	provisioner, c := newValkey(t)
	ctx := context.Background()
	legacy := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name:      "kitchen-cache",
		Namespace: DefaultCacheNamespace,
		Labels:    map[string]string{instanceLabel: "kitchen-cache", managedByLabel: managedByValue},
	}}
	if err := c.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	_, err := provisioner.Provision(ctx, shopCache)
	if !errors.Is(err, naming.ErrNotAdoptable) {
		t.Fatalf("want ErrNotAdoptable, got %v", err)
	}
	if !strings.Contains(err.Error(), naming.AdoptAnnotation) {
		t.Errorf("the refusal does not say how to hand it over: %v", err)
	}

	handed := naming.Resource{Project: "shop", Claim: "cache", HandOver: "kitchen-cache"}
	if _, err := provisioner.Provision(ctx, handed); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want the handed-over instance, still starting, got %v", err)
	}
	adopted := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: DefaultCacheNamespace, Name: "kitchen-cache"}
	if err := c.Get(ctx, key, adopted); err != nil {
		t.Fatal(err)
	}
	if got := adopted.Labels[naming.LabelProject]; got != "shop" {
		t.Fatalf("the handed-over instance records project %q", got)
	}
}

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

package inngest

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// The claim these tests provision for: the server's name is
// kitchen-<project>-<claim>.
var shopJobs = naming.Resource{Project: "shop", Claim: "jobs"}

const (
	serverName  = "kitchen-shop-jobs"
	previewName = "kitchen-shop-jobs-shop-pr-7"
)

// storage is the two in-cluster provisioners as this provider uses them,
// faked: production's Postgres and queue are provisioned through the same
// code paths a postgres and a redis claim go through, and what this file is
// about is what the Inngest server is *told*, not how CloudNativePG makes a
// database.
type storage struct {
	postgresURL, redisURL string
	notReady              bool
	provisioned           []string
	deprovisioned         []string
	usage                 cache.Usage
}

func (s *storage) ProvisionWith(
	_ context.Context,
	res naming.Resource,
	_ database.Requirements,
) (database.Instance, error) {
	if s.notReady {
		return database.Instance{}, database.ErrNotReady
	}
	s.provisioned = append(s.provisioned, res.Name)
	return database.Instance{ID: "ns/" + res.Name, Binding: database.Binding{URL: s.postgresURL}}, nil
}

func (s *storage) provisionCache(
	_ context.Context,
	res naming.Resource,
	req cache.Requirements,
) (cache.Instance, error) {
	if s.notReady {
		return cache.Instance{}, cache.ErrNotReady
	}
	s.usage = req.Usage
	s.provisioned = append(s.provisioned, res.Name)
	return cache.Instance{ID: "ns/" + res.Name, Binding: cache.Binding{URL: s.redisURL}}, nil
}

func (s *storage) Deprovision(_ context.Context, instanceID string) error {
	s.deprovisioned = append(s.deprovisioned, instanceID)
	return nil
}

// cacheHalf adapts the same fake to the cache side of the contract, so that
// one object records both halves of what a server was given.
type cacheHalf struct{ *storage }

func (c cacheHalf) ProvisionWith(
	ctx context.Context,
	res naming.Resource,
	req cache.Requirements,
) (cache.Instance, error) {
	return c.storage.provisionCache(ctx, res, req)
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, kitchenv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func newSelfHosted(t *testing.T) (*SelfHosted, client.Client, *storage) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	store := &storage{
		postgresURL: "postgres://app:secret@kitchen-shop-jobs-db-rw.kitchen-inngest.svc:5432/app",
		redisURL:    "redis://:secret@kitchen-shop-jobs-queue.kitchen-inngest.svc:6379",
	}
	provisioner, err := NewSelfHosted(Options{
		Connection: connectionNamed(ProviderSelfHosted, ""),
		Cluster:    c,
		Postgres:   store,
		Cache:      cacheHalf{store},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provisioner, c, store
}

// serve drives a server to ready the way a cluster would: the first pass
// creates it and answers ErrNotReady, and the Deployment's status is what
// the second pass reads.
func markReady(t *testing.T, c client.Client, name string) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: name}
	if err := c.Get(context.Background(), key, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ReadyReplicas = 1
	if err := c.Status().Update(context.Background(), deployment); err != nil {
		t.Fatal(err)
	}
}

func deploymentOf(t *testing.T, c client.Client, name string) *appsv1.Deployment {
	t.Helper()
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: name}
	if err := c.Get(context.Background(), key, deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

func envOf(deployment *appsv1.Deployment) map[string]string {
	env := map[string]string{}
	for _, v := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	return env
}

// Production's server is the claim's own: a Deployment, a Service, a Secret,
// and the Postgres and the queue the Inngest docs ask for — provisioned
// through the platform's own in-cluster providers rather than a second
// implementation of each.
func TestProductionsServerRunsOnExternalStorage(t *testing.T) {
	provisioner, c, store := newSelfHosted(t)
	ctx := context.Background()

	if _, err := provisioner.Provision(ctx, shopJobs, Requirements{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a server that has just been created is not ready yet, got %v", err)
	}
	markReady(t, c, serverName)

	instance, err := provisioner.Provision(ctx, shopJobs, Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != DefaultServerNamespace+"/"+serverName || instance.Name != serverName {
		t.Fatalf("the instance is the namespaced server: %+v", instance)
	}
	if instance.Environment != "" {
		t.Errorf("a self-hosted server has no Inngest environment, got %q", instance.Environment)
	}

	// The binding is the server's address and its own keys — minted here,
	// because it is this server that checks them.
	binding := instance.Binding
	wantBase := "http://" + serverName + "." + DefaultServerNamespace + ".svc:8288"
	if binding.BaseURL != wantBase {
		t.Errorf("INNGEST_BASE_URL is the server's address: %q, want %q", binding.BaseURL, wantBase)
	}
	if binding.Dev != "0" {
		t.Errorf("a self-hosted server is reached in cloud mode: INNGEST_DEV is %q", binding.Dev)
	}
	if !strings.HasPrefix(binding.ConnectGatewayURL, "ws://"+serverName) ||
		!strings.HasSuffix(binding.ConnectGatewayURL, ":8289/v0/connect") {
		t.Errorf("the connect gateway is on its own port: %q", binding.ConnectGatewayURL)
	}
	if binding.Env != "" {
		t.Errorf("there are no environments to select: INNGEST_ENV is %q", binding.Env)
	}
	if len(binding.EventKey) != 64 || len(binding.SigningKey) != 64 || binding.EventKey == binding.SigningKey {
		t.Errorf("the key pair is two 32-byte hex values: %+v", binding)
	}

	// Storage: one Postgres and one queue, named apart from the server so
	// that three objects do not fight over one name — and the queue asks for
	// `queue`, because what is in it is function runs nobody can recompute.
	// Two passes, each asking for both — the storage is looked up by name on
	// every reconcile, which is what makes provisioning restartable.
	if len(store.provisioned) != 4 ||
		store.provisioned[0] != serverName+"-db" || store.provisioned[1] != serverName+"-queue" {
		t.Fatalf("production's server gets a Postgres and a queue of its own: %v", store.provisioned)
	}
	if store.usage != cache.UsageQueue {
		t.Errorf("Inngest's Redis holds work nobody can recompute, so it is a queue: %q", store.usage)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: serverName}
	if err := c.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data[serverKeyPostgresURI]) != store.postgresURL ||
		string(secret.Data[serverKeyRedisURI]) != store.redisURL {
		t.Errorf("the server is told where its storage is: %v", secret.Data)
	}

	deployment := deploymentOf(t, c, serverName)
	if got := envOf(deployment)["INNGEST_SDK_URL"]; got != "" {
		t.Errorf("a connect claim tells the server no URL to call: %q", got)
	}
	volume := deployment.Spec.Template.Spec.Volumes[0]
	if volume.EmptyDir == nil {
		t.Errorf("a server on external storage keeps nothing of its own: %+v", volume)
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != DefaultServerImage {
		t.Errorf("the image is the pinned one: %q", deployment.Spec.Template.Spec.Containers[0].Image)
	}
}

// A preview gets a server of its own — the whole of the tenancy answer — on
// the embedded store, which is the one respect in which it is not
// production's shape.
func TestAPreviewGetsItsOwnServerOnTheEmbeddedStore(t *testing.T) {
	provisioner, c, store := newSelfHosted(t)
	ctx := context.Background()

	_, _ = provisioner.Provision(ctx, shopJobs, Requirements{})
	markReady(t, c, serverName)
	instance, err := provisioner.Provision(ctx, shopJobs, Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	provisionedForProduction := len(store.provisioned)

	if _, err := provisioner.CreateBranch(ctx, instance.ID, "shop-pr-7", Requirements{}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("a preview's server that has just been created is not ready yet, got %v", err)
	}
	markReady(t, c, previewName)
	branch, err := provisioner.CreateBranch(ctx, instance.ID, "shop-pr-7", Requirements{})
	if err != nil {
		t.Fatal(err)
	}
	if branch.ID != DefaultServerNamespace+"/"+previewName {
		t.Fatalf("the preview's server is beside the claim's: %q", branch.ID)
	}
	if branch.Binding.BaseURL == instance.Binding.BaseURL {
		t.Error("a preview's own server is a different address from production's, or it is not its own")
	}
	if branch.Binding.EventKey == instance.Binding.EventKey {
		t.Error("a preview's own server has its own keys: an event sent with production's must not reach it")
	}
	if len(store.provisioned) != provisionedForProduction {
		t.Errorf("a preview's server keeps its own state: it asks for no Postgres and no queue, got %v",
			store.provisioned)
	}

	deployment := deploymentOf(t, c, previewName)
	volume := deployment.Spec.Template.Spec.Volumes[0]
	if volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != previewName {
		t.Fatalf("the embedded store is on a volume of its own: %+v", volume)
	}
	if got := envOf(deployment)["INNGEST_SQLITE_DIR"]; got != dataDir {
		t.Errorf("the embedded store is written to the volume: %q", got)
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: previewName}
	if err := c.Get(ctx, key, claim); err != nil {
		t.Fatal(err)
	}
}

// Serve mode is where the platform has something to tell the server: the
// environment's own URL, which nothing in the repository could have written
// down, and to look again after every deploy.
func TestServeModeTellsTheServerWhereToCall(t *testing.T) {
	provisioner, c, _ := newSelfHosted(t)
	ctx := context.Background()
	req := Requirements{Mode: ModeServe, ServeURL: "https://shop.apps.example.com/api/inngest"}

	_, _ = provisioner.Provision(ctx, shopJobs, req)
	env := envOf(deploymentOf(t, c, serverName))
	if env["INNGEST_SDK_URL"] != req.ServeURL {
		t.Fatalf("a serve claim tells the server where the application is: %q", env["INNGEST_SDK_URL"])
	}
	if env["INNGEST_POLL_INTERVAL"] == "" || env["INNGEST_POLL_INTERVAL"] == "0" {
		t.Errorf("a server that never polls holds the function set of the first sync forever: %q",
			env["INNGEST_POLL_INTERVAL"])
	}

	// The environment moves — a first deploy publishes it, a domain changes
	// — and the server is rolled onto the new address rather than left
	// polling the old one.
	before := deploymentOf(t, c, serverName).Annotations[configAnnotation]
	req.ServeURL = "https://shop.example.com/jobs/inngest"
	_, _ = provisioner.Provision(ctx, shopJobs, req)
	after := deploymentOf(t, c, serverName)
	if after.Annotations[configAnnotation] == before {
		t.Fatal("the config digest did not move, so the pods would never pick up the new URL")
	}
	if envOf(after)["INNGEST_SDK_URL"] != req.ServeURL {
		t.Errorf("the server was not told the new URL: %q", envOf(after)["INNGEST_SDK_URL"])
	}
}

// Parking is the replica count and nothing else, in both directions, and it
// leaves the volume the preview's runs are on exactly where it is.
func TestIdlingAPreviewParksItsServerAndWakingBringsItBack(t *testing.T) {
	provisioner, c, _ := newSelfHosted(t)
	ctx := context.Background()
	_, _ = provisioner.Provision(ctx, shopJobs, Requirements{})
	markReady(t, c, serverName)
	instance, _ := provisioner.Provision(ctx, shopJobs, Requirements{})
	_, _ = provisioner.CreateBranch(ctx, instance.ID, "shop-pr-7", Requirements{})
	branchID := DefaultServerNamespace + "/" + previewName

	if err := provisioner.IdleBranch(ctx, branchID); err != nil {
		t.Fatal(err)
	}
	if got := *deploymentOf(t, c, previewName).Spec.Replicas; got != 0 {
		t.Fatalf("a parked preview's server runs no pods, got %d", got)
	}
	claim := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: previewName}
	if err := c.Get(ctx, key, claim); err != nil {
		t.Fatalf("parking is a park and not a teardown: the volume went with it: %v", err)
	}

	if err := provisioner.WakeBranch(ctx, branchID); err != nil {
		t.Fatal(err)
	}
	if got := *deploymentOf(t, c, previewName).Spec.Replicas; got != 1 {
		t.Fatalf("a woken preview's server runs again, got %d", got)
	}

	// Both directions are idempotent, and both tolerate a branch that is no
	// longer there: a park that failed would wedge a claim, and the worst
	// case it protects against is a preview that keeps running.
	if err := provisioner.WakeBranch(ctx, branchID); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.IdleBranch(ctx, DefaultServerNamespace+"/gone"); err != nil {
		t.Fatal(err)
	}
}

// A reconcile that changes nothing writes nothing: the config digest is what
// stops every pass rolling the pods.
func TestASecondReconcileWritesNothing(t *testing.T) {
	provisioner, c, _ := newSelfHosted(t)
	ctx := context.Background()
	_, _ = provisioner.Provision(ctx, shopJobs, Requirements{})
	markReady(t, c, serverName)
	if _, err := provisioner.Provision(ctx, shopJobs, Requirements{}); err != nil {
		t.Fatal(err)
	}
	first := deploymentOf(t, c, serverName).ResourceVersion
	if _, err := provisioner.Provision(ctx, shopJobs, Requirements{}); err != nil {
		t.Fatal(err)
	}
	if deploymentOf(t, c, serverName).ResourceVersion != first {
		t.Error("a reconcile that changes nothing rewrote the Deployment, which would roll the pods")
	}
}

// Deleting takes everything back, including the storage: the claim type
// carries no deletionPolicy, so this is the whole of what deletion means.
func TestDeprovisionDestroysTheServerAndItsStorage(t *testing.T) {
	provisioner, c, store := newSelfHosted(t)
	ctx := context.Background()
	_, _ = provisioner.Provision(ctx, shopJobs, Requirements{})
	markReady(t, c, serverName)
	instance, _ := provisioner.Provision(ctx, shopJobs, Requirements{})

	if err := provisioner.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: serverName}
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.Secret{}} {
		if err := c.Get(ctx, key, object); !apierrors.IsNotFound(err) {
			t.Errorf("%T survived the claim: %v", object, err)
		}
	}
	if len(store.deprovisioned) != 2 {
		t.Fatalf("the Postgres and the queue go with the server: %v", store.deprovisioned)
	}
	// Deleting again is not an error: a finalizer runs more than once.
	if err := provisioner.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
}

// What this provider cannot do, it refuses before it creates anything, and
// the message says which connection could.
func TestSelfHostedRefusesWhatItCannotServe(t *testing.T) {
	provisioner, c, _ := newSelfHosted(t)
	ctx := context.Background()

	_, err := provisioner.Provision(ctx, shopJobs, Requirements{Environment: "staging"})
	if !errors.Is(err, ErrUnsatisfiable) || !strings.Contains(err.Error(), "no environments") {
		t.Fatalf("an Inngest environment is not something a self-hosted server has: %v", err)
	}
	_, err = provisioner.Provision(ctx, shopJobs, Requirements{Mode: "webhook"})
	if !errors.Is(err, ErrUnsatisfiable) || !strings.Contains(err.Error(), ModeServe) {
		t.Fatalf("an unknown mode is refused, naming the two that are not: %v", err)
	}
	deployment := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: DefaultServerNamespace, Name: serverName}
	if err := c.Get(ctx, key, deployment); !apierrors.IsNotFound(err) {
		t.Error("a refusal created a server anyway")
	}

	// Serve is not refused: it is the mode a server in this cluster exists
	// to make possible.
	if _, err := provisioner.Provision(ctx, shopJobs, Requirements{Mode: ModeServe}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("serve mode is provisioned by a self-hosted server: %v", err)
	}
}

// A storage instance that is still starting holds the claim Pending rather
// than failing it — and nothing is bound until the server behind it is up.
func TestStorageThatIsStartingHoldsTheClaimPending(t *testing.T) {
	provisioner, _, store := newSelfHosted(t)
	store.notReady = true

	_, err := provisioner.Provision(context.Background(), shopJobs, Requirements{})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("a database that is starting is not a claim that failed: %v", err)
	}
	if !strings.Contains(err.Error(), "Postgres") {
		t.Errorf("the message says what is starting: %v", err)
	}
}

// A server another project already has under this name is not adopted, which
// is what stops one project's claim binding to another's event history.
func TestAServerOfAnotherProjectIsNotAdopted(t *testing.T) {
	provisioner, c, _ := newSelfHosted(t)
	ctx := context.Background()
	other := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      serverName,
		Namespace: DefaultServerNamespace,
		Labels:    map[string]string{naming.LabelProject: "warehouse"},
	}}
	if err := c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := provisioner.Provision(ctx, shopJobs, Requirements{}); !errors.Is(err, naming.ErrNotAdoptable) {
		t.Fatalf("a server belonging to another project must not be bound: %v", err)
	}
}

// The Connection is the operator's lever over every server this installation
// runs — the image above all, which is what the platform knows how to
// operate.
func TestTheConnectionCarriesTheOperatorsDefaults(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	provisioner, err := NewSelfHosted(Options{
		Connection: connectionNamed(ProviderSelfHosted,
			`{"namespace": "jobs", "image": "registry.internal/inngest:v1.44.0", "storageSize": "4Gi", "storageClass": "fast"}`),
		Cluster:  c,
		Postgres: &storage{},
		Cache:    cacheHalf{&storage{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if provisioner.Namespace != "jobs" || provisioner.Image != "registry.internal/inngest:v1.44.0" ||
		provisioner.StorageSize != "4Gi" || provisioner.StorageClass != "fast" {
		t.Fatalf("the connection's config is what every claim through it inherits: %+v", provisioner)
	}
	if _, err := NewSelfHosted(Options{Connection: connectionNamed(ProviderSelfHosted, `{`), Cluster: c}); err == nil {
		t.Error("a config the provisioner cannot read is a connection that is wrong, not a default")
	}
	if _, err := NewSelfHosted(Options{Connection: connectionNamed(ProviderSelfHosted, "")}); err == nil {
		t.Error("this provider runs Inngest in the cluster and cannot do it without a client")
	}
}

// Default resolves the provider name the Connection carries, and the two
// implementations are not interchangeable: one holds an API key, the other
// holds a cluster.
func TestDefaultResolvesBothProviders(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	selfHosted, err := Default(Options{Connection: connectionNamed(ProviderSelfHosted, ""), Cluster: c})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := selfHosted.(*SelfHosted); !ok {
		t.Fatalf("%s resolves to the in-cluster provisioner, got %T", ProviderSelfHosted, selfHosted)
	}
	// The two optional interfaces the reconciler asks about, which are the
	// whole of what differs between the providers downstream.
	if _, ok := selfHosted.(Deprovisioner); !ok {
		t.Error("a server this platform runs is this platform's to destroy")
	}
	if _, ok := selfHosted.(IdlingProvisioner); !ok {
		t.Error("a preview's own server parks with the preview, which is what bounds the cost of having one")
	}
	if _, ok := selfHosted.(AppReporter); ok {
		t.Error("a self-hosted server publishes no app inventory, and must not pretend to")
	}

	cloud, err := Default(Options{Connection: connectionNamed(ProviderCloud, ""), Token: "sk-inn-api-x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cloud.(AppReporter); !ok {
		t.Error("Inngest Cloud answers GET /apps and the claim reports it")
	}
	if _, ok := cloud.(Deprovisioner); ok {
		t.Error("nothing an Inngest Cloud claim binds is the platform's to destroy")
	}
	if _, ok := cloud.(IdlingProvisioner); ok {
		t.Error("a branch environment is Inngest's to run, and this platform has no lever on it")
	}
}

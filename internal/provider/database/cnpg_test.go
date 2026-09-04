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

package database

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

const (
	testDatabaseNamespace = "kitchen-databases"
	// testCluster is the one database these tests provision; postgisImage16
	// is what the postgis build resolves to for the major they ask for.
	testCluster    = "kitchen-shop-db"
	postgisImage16 = "ghcr.io/cloudnative-pg/postgis:16"
)

// shopDB is the claim these tests provision for: project "shop", claim "db",
// which is what makes the Cluster kitchen-shop-db.
var shopDB = naming.Resource{Project: shopProject, Claim: "db"}

// shopProject is the project every claim in these tests belongs to, and what
// the Cluster records as its owner.
const shopProject = "shop"

// cnpgScheme knows the two kinds this provisioner touches and CloudNativePG's
// own, as an unstructured type — the same way the provisioner addresses it,
// and so without importing cnpg's module into the build.
func cnpgScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	gvk := clusterGVK()
	scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
	return scheme
}

func cnpgAgainstFakeCluster(t *testing.T, objects ...client.Object) *CNPG {
	t.Helper()
	scheme := cnpgScheme(t)
	return &CNPG{
		Client:      fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace:   testDatabaseNamespace,
		Images:      DefaultPostgresImages,
		StorageSize: DefaultStorageSize,
		Instances:   DefaultInstances,
	}
}

// readyCluster is what CloudNativePG's own reconciler eventually writes; the
// fake cluster has no cnpg in it, so the test plays that part.
func readyCluster() *unstructured.Unstructured {
	cluster := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"phase": "Cluster in healthy state",
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True", "reason": "ClusterIsReady"},
			},
		},
	}}
	cluster.SetGroupVersionKind(clusterGVK())
	cluster.SetNamespace(testDatabaseNamespace)
	cluster.SetName(testCluster)
	return cluster
}

func appSecret(cluster string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cluster + "-app", Namespace: testDatabaseNamespace},
		Data: map[string][]byte{
			"username": []byte("app"),
			"password": []byte("s3cr#t/pass"),
			"dbname":   []byte("app"),
			// cnpg writes the Service's short name here, which resolves in
			// this namespace and nowhere else. The provisioner is expected to
			// ignore it.
			"host": []byte(cluster + "-rw"),
			"port": []byte("5432"),
		},
	}
}

func getCluster(t *testing.T, c *CNPG, name string) *unstructured.Unstructured {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: name}
	if err := c.Client.Get(context.Background(), key, cluster); err != nil {
		t.Fatalf("reading cluster %s: %v", name, err)
	}
	return cluster
}

// A database the platform runs takes minutes to come up, and the claim waits
// Pending rather than reading Failed for every one of them.
func TestProvisionCreatesTheClusterAndReportsItNotReady(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t)

	_, err := cnpg.Provision(context.Background(), shopDB)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}

	cluster := getCluster(t, cnpg, "kitchen-shop-db")
	if image := nestedString(cluster, "spec", "imageName"); image != "ghcr.io/cloudnative-pg/postgresql:"+DefaultPostgresMajor {
		t.Fatalf("unexpected image %q", image)
	}
	if size := nestedString(cluster, "spec", "storage", "size"); size != DefaultStorageSize {
		t.Fatalf("storage size %q, want the default", size)
	}
	if database := nestedString(cluster, "spec", "bootstrap", "initdb", "database"); database != applicationDatabase {
		t.Fatalf("bootstrap database %q", database)
	}
	if cluster.GetLabels()[managedByLabel] != managedByValue {
		t.Fatal("the cluster is not marked as the platform's")
	}
}

func TestProvisionBindsOnceTheClusterIsServing(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t, readyCluster(), appSecret("kitchen-shop-db"))

	instance, err := cnpg.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != testDatabaseNamespace+"/kitchen-shop-db" {
		t.Fatalf("instance ID %q", instance.ID)
	}
	if instance.Provenance != ProvenanceProduction {
		t.Fatalf("provenance %q, want production", instance.Provenance)
	}
	// The host has to be the fully qualified one: every consumer of this
	// binding is a pod in some project's own namespace, where the short name
	// cnpg wrote resolves to nothing.
	want := "kitchen-shop-db-rw." + testDatabaseNamespace + ".svc"
	if instance.Binding.Host != want {
		t.Fatalf("host %q, want %q", instance.Binding.Host, want)
	}
	if !strings.Contains(instance.Binding.URL, want) {
		t.Fatalf("connection URL %q does not reach the database", instance.Binding.URL)
	}
	// A password with URL-significant characters in it has to survive being
	// put in a URL, or the application gets a string it cannot connect with.
	if strings.Contains(instance.Binding.URL, "s3cr#t/pass") {
		t.Fatalf("the password was not escaped into the URL: %q", instance.Binding.URL)
	}
	if instance.Binding.Password != "s3cr#t/pass" {
		t.Fatalf("password %q", instance.Binding.Password)
	}
}

// libpq's default is `prefer`, which negotiates TLS and falls back to
// plaintext without saying so — on a cluster with no network policy that is
// every application's credentials and every row on the wire between two
// namespaces. CloudNativePG serves TLS on every cluster it creates, so the
// binding asks for it.
func TestTheBindingRequiresTLS(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t, readyCluster(), appSecret("kitchen-shop-db"))

	instance, err := cnpg.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := url.Parse(instance.Binding.URL)
	if err != nil {
		t.Fatalf("connection URL %q does not parse: %v", instance.Binding.URL, err)
	}
	if mode := parsed.Query().Get("sslmode"); mode != "require" {
		t.Fatalf("sslmode %q, want require (URL %q)", mode, instance.Binding.URL)
	}
}

func TestExtensionsAreCreatedAtBootstrapRatherThanLeftToTheApplication(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t)

	_, err := cnpg.ProvisionWith(context.Background(), naming.Resource{Project: "shop", Claim: "maps"}, Requirements{
		Version:      "16",
		Extensions:   []string{"postgis", "pgvector"},
		StorageSize:  "40Gi",
		StorageClass: "fast",
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}

	cluster := getCluster(t, cnpg, "kitchen-shop-maps")
	if image := nestedString(cluster, "spec", "imageName"); image != postgisImage16 {
		t.Fatalf("image %q, want the postgis build", image)
	}
	if size := nestedString(cluster, "spec", "storage", "size"); size != "40Gi" {
		t.Fatalf("storage size %q", size)
	}
	if class := nestedString(cluster, "spec", "storage", "storageClass"); class != "fast" {
		t.Fatalf("storage class %q", class)
	}
	statements, found, err := unstructured.NestedStringSlice(cluster.Object,
		"spec", "bootstrap", "initdb", "postInitApplicationSQL")
	if err != nil || !found {
		t.Fatalf("no bootstrap SQL: %v", err)
	}
	joined := strings.Join(statements, "\n")
	if !strings.Contains(joined, `CREATE EXTENSION IF NOT EXISTS "postgis"`) ||
		!strings.Contains(joined, `CREATE EXTENSION IF NOT EXISTS "vector"`) {
		t.Fatalf("unexpected bootstrap SQL: %q", joined)
	}
}

// Nothing is created: the whole point of resolving capabilities is that an
// unsatisfiable claim fails as a claim, before there is a database to clean
// up.
func TestAnUnsatisfiableClaimIsRefusedWithoutCreatingAnything(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t)

	_, err := cnpg.ProvisionWith(context.Background(), naming.Resource{Project: "shop", Claim: "metrics"}, Requirements{
		Extensions: []string{"timescaledb"},
	})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("error %v, want ErrUnsatisfiable", err)
	}
	if !strings.Contains(err.Error(), "timescaledb") {
		t.Fatalf("the refusal does not name what it could not supply: %q", err.Error())
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: "kitchen-shop-metrics"}
	if err := cnpg.Client.Get(context.Background(), key, cluster); !apierrors.IsNotFound(err) {
		t.Fatalf("a cluster was created for a claim that was refused (%v)", err)
	}
}

// The honest answer to previews: a preview gets its own empty database, and
// says so. No copy-on-write branch exists here, and a pg_basebackup of
// production into a preview is the thing this design exists to prevent.
func TestAPreviewGetsItsOwnEmptyDatabaseAndDeclaresItSynthetic(t *testing.T) {
	parent := readyCluster()
	if err := unstructured.SetNestedField(parent.Object,
		postgisImage16, "spec", "imageName"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(parent.Object, "40Gi", "spec", "storage", "size"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedStringSlice(parent.Object,
		[]string{`CREATE EXTENSION IF NOT EXISTS "postgis"`},
		"spec", "bootstrap", "initdb", "postInitApplicationSQL"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(parent.Object, applicationDatabase,
		"spec", "bootstrap", "initdb", "database"); err != nil {
		t.Fatal(err)
	}

	cnpg := cnpgAgainstFakeCluster(t, parent, appSecret("kitchen-shop-db-pr-7"))
	// First pass: the branch cluster is created and is not ready.
	if _, err := cnpg.CreateBranch(context.Background(), testDatabaseNamespace+"/kitchen-shop-db", "pr-7"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("error %v, want ErrNotReady", err)
	}
	created := getCluster(t, cnpg, "kitchen-shop-db-pr-7")
	if image := nestedString(created, "spec", "imageName"); image != postgisImage16 {
		t.Fatalf("branch image %q, want the parent's", image)
	}
	if size := nestedString(created, "spec", "storage", "size"); size != "40Gi" {
		t.Fatalf("branch storage %q, want the parent's", size)
	}
	// The parent's *bootstrap* is inherited, which is what makes the preview
	// the same database shape; its data is not, because initdb makes a new
	// one. Nothing here restores from the parent.
	if _, found, _ := unstructured.NestedMap(created.Object, "spec", "bootstrap", "pg_basebackup"); found {
		t.Fatal("the branch bootstraps from the parent — a preview must not carry production data")
	}
	if _, found, _ := unstructured.NestedMap(created.Object, "spec", "bootstrap", "recovery"); found {
		t.Fatal("the branch recovers from a backup — a preview must not carry production data")
	}
	statements, _, _ := unstructured.NestedStringSlice(created.Object,
		"spec", "bootstrap", "initdb", "postInitApplicationSQL")
	if len(statements) != 1 || !strings.Contains(statements[0], "postgis") {
		t.Fatalf("the branch did not inherit the parent's extensions: %v", statements)
	}

	// Second pass: cnpg has made it ready, so the branch binds.
	if err := unstructured.SetNestedSlice(created.Object, []any{
		map[string]any{"type": "Ready", "status": "True", "reason": "ClusterIsReady"},
	}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := cnpg.Client.Update(context.Background(), created); err != nil {
		t.Fatal(err)
	}
	branch, err := cnpg.CreateBranch(context.Background(), testDatabaseNamespace+"/kitchen-shop-db", "pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if branch.Provenance != ProvenanceSynthetic {
		t.Fatalf("provenance %q, want synthetic — the preview's database is empty", branch.Provenance)
	}
	if branch.ID != testDatabaseNamespace+"/kitchen-shop-db-pr-7" {
		t.Fatalf("branch ID %q", branch.ID)
	}
}

func TestDeprovisionDeletesTheClusterAndToleratesAnAbsentOne(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t, readyCluster())
	ctx := context.Background()

	if err := cnpg.Deprovision(ctx, testDatabaseNamespace+"/kitchen-shop-db"); err != nil {
		t.Fatal(err)
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: "kitchen-shop-db"}
	if err := cnpg.Client.Get(ctx, key, cluster); !apierrors.IsNotFound(err) {
		t.Fatalf("the cluster survived deprovisioning (%v)", err)
	}
	// Deleting a claim must not wedge on a database that is already gone.
	if err := cnpg.Deprovision(ctx, testDatabaseNamespace+"/kitchen-shop-db"); err != nil {
		t.Fatalf("deprovisioning an absent database failed: %v", err)
	}
	if err := cnpg.DeleteBranch(ctx, "", ""); err != nil {
		t.Fatalf("deleting an unrecorded branch failed: %v", err)
	}
}

// Residency is reported, never declared: it comes off the node the primary
// actually landed on, and an unlabelled node reports nothing rather than a
// guess.
func TestResidencyIsReadOffThePrimarysNode(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "worker-3",
		Labels: map[string]string{regionLabel: "eu-central-2", zoneLabel: "eu-central-2a"},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-shop-db-1",
			Namespace: testDatabaseNamespace,
			Labels:    map[string]string{clusterLabel: "kitchen-shop-db", "role": "primary"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-3"},
	}
	cnpg := cnpgAgainstFakeCluster(t, readyCluster(), appSecret("kitchen-shop-db"), pod, node)

	instance, err := cnpg.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Region != "eu-central-2" {
		t.Fatalf("residency %q, want the node's region", instance.Region)
	}
}

func TestResidencyIsEmptyWhereTheClusterSaysNothingAboutItsTopology(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-3"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-shop-db-1",
			Namespace: testDatabaseNamespace,
			Labels:    map[string]string{clusterLabel: "kitchen-shop-db"},
		},
		Spec: corev1.PodSpec{NodeName: "worker-3"},
	}
	cnpg := cnpgAgainstFakeCluster(t, readyCluster(), appSecret("kitchen-shop-db"), pod, node)

	instance, err := cnpg.Provision(context.Background(), shopDB)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Region != "" {
		t.Fatalf("residency %q, want nothing — the node declares no topology", instance.Region)
	}
}

func TestAConnectionsConfigSuppliesTheDefaultsEveryClaimInherits(t *testing.T) {
	cnpg, err := NewCNPG(Options{
		Connection: connectionWithConfig(t, `{
			"namespace": "databases",
			"storageSize": "50Gi",
			"storageClass": "ceph",
			"instances": 3,
			"images": [{"repository": "registry.internal/pg", "majors": ["16"], "extensions": ["timescaledb"]}]
		}`),
		Cluster: fake.NewClientBuilder().WithScheme(cnpgScheme(t)).Build(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cnpg.Namespace != "databases" || cnpg.StorageSize != "50Gi" ||
		cnpg.StorageClass != "ceph" || cnpg.Instances != 3 {
		t.Fatalf("unexpected defaults: %+v", cnpg)
	}
	if len(cnpg.Images) != 1 || cnpg.Images[0].Repository != "registry.internal/pg" {
		t.Fatalf("unexpected catalogue: %+v", cnpg.Images)
	}
}

// connectionWithConfig is a cnpg Connection carrying the given spec.config.
func connectionWithConfig(t *testing.T, config string) *kitchenv1alpha1.Connection {
	t.Helper()
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: "kitchen-system"},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: ProviderCNPG},
	}
	if config != "" {
		conn.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
	}
	return conn
}

// The worst thing a naming rule here can do is put two projects in one
// database, which plain truncation would do to two long names sharing a
// prefix.
func TestTwoLongClaimNamesDoNotLandOnOneDatabase(t *testing.T) {
	long := strings.Repeat("a", 40)
	left := naming.Resource{Project: "shop", Claim: long + "-orders"}.Qualified(maxClusterName)
	right := naming.Resource{Project: "shop", Claim: long + "-billing"}.Qualified(maxClusterName)

	if left == right {
		t.Fatalf("two claims resolved to the same database: %q", left)
	}
	for _, name := range []string{left, right} {
		if len(name) > maxClusterName {
			t.Fatalf("%q is longer than cnpg will take", name)
		}
		if strings.HasSuffix(name, "-") || strings.HasPrefix(name, "-") {
			t.Fatalf("%q is not a DNS label", name)
		}
	}
	// A name that fits is left exactly as it is — the digest is what replaces
	// what was cut, not decoration.
	if got := shopDB.Qualified(maxClusterName); got != "kitchen-shop-db" {
		t.Fatalf("a name that fits was rewritten to %q", got)
	}
}

func TestAPreviewsDatabaseIsNamedAfterItsParentAndItsEnvironment(t *testing.T) {
	name := branchName("kitchen-shop-db", "kitchen-shop-pr-41")
	if !strings.HasPrefix(name, "kitchen-shop-db-") || !strings.Contains(name, "pr-41") {
		t.Fatalf("unexpected branch name %q", name)
	}
	if len(name) > maxClusterName {
		t.Fatalf("%q is longer than cnpg will take", name)
	}
}

func TestTheProvisionerRefusesToBeBuiltWithoutACluster(t *testing.T) {
	if _, err := NewCNPG(Options{Connection: connectionWithConfig(t, "")}); err == nil {
		t.Fatal("a provisioner with nothing to provision into was built")
	}
}

// namedCluster is a Cluster somebody's earlier claim left behind, ready and
// carrying whatever project it was recorded for — none, for one provisioned
// before names carried the project.
func namedCluster(name, project string) *unstructured.Unstructured {
	cluster := readyCluster()
	cluster.SetName(name)
	labels := map[string]string{managedByLabel: managedByValue}
	if project != "" {
		labels[naming.LabelProject] = project
	}
	cluster.SetLabels(labels)
	return cluster
}

// A database is named after the project as well as the claim, and records
// the project on itself — the two halves of not handing one project's data
// to another.
func TestADatabaseCarriesTheProjectThatClaimedIt(t *testing.T) {
	cnpg := cnpgAgainstFakeCluster(t)

	if _, err := cnpg.Provision(context.Background(), shopDB); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	cluster := getCluster(t, cnpg, "kitchen-shop-db")
	if got := cluster.GetLabels()[naming.LabelProject]; got != shopProject {
		t.Fatalf("the cluster records project %q", got)
	}
}

// The bug: under the default Retain a deleted claim leaves its database
// behind, and a claim of the same name in another project used to be bound
// to it.
func TestAnotherProjectsRetainedDatabaseIsNotAdopted(t *testing.T) {
	retained := namedCluster("kitchen-shop-db", "shop")
	cnpg := cnpgAgainstFakeCluster(t, retained, appSecret("kitchen-shop-db"))

	_, err := cnpg.Provision(context.Background(), naming.Resource{Project: "warehouse", Claim: "db"})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("want a database of its own, still starting, got %v", err)
	}
	mine := getCluster(t, cnpg, "kitchen-warehouse-db")
	if got := mine.GetLabels()[naming.LabelProject]; got != "warehouse" {
		t.Fatalf("the second project got a database recorded as %q", got)
	}
	// And the first project's is untouched.
	if got := getCluster(t, cnpg, "kitchen-shop-db").GetLabels()[naming.LabelProject]; got != shopProject {
		t.Fatalf("the retained database was rewritten to project %q", got)
	}
}

// An orphan from before the project was in the name is not taken silently by
// anybody: nothing records whose data is in it, so the claim fails saying so.
func TestADatabaseNamedBeforeTheProjectIsRefusedUntilItIsHandedOver(t *testing.T) {
	legacy := namedCluster("kitchen-db", "")
	cnpg := cnpgAgainstFakeCluster(t, legacy, appSecret("kitchen-db"))
	ctx := context.Background()

	_, err := cnpg.Provision(ctx, naming.Resource{Project: shopProject, Claim: "db"})
	if !errors.Is(err, naming.ErrNotAdoptable) {
		t.Fatalf("want ErrNotAdoptable, got %v", err)
	}
	for _, says := range []string{"kitchen-db", naming.AdoptAnnotation, "provisioned before"} {
		if !strings.Contains(err.Error(), says) {
			t.Errorf("the refusal does not say %q: %v", says, err)
		}
	}
	// Nothing was created in its place while the claim was refused.
	stray := &unstructured.Unstructured{}
	stray.SetGroupVersionKind(clusterGVK())
	key := types.NamespacedName{Namespace: testDatabaseNamespace, Name: "kitchen-shop-db"}
	if err := cnpg.Client.Get(ctx, key, stray); err == nil {
		t.Fatal("a second database was created while the claim was refused")
	}

	// The operator hands it over by naming it, and the database records the
	// project from then on.
	instance, err := cnpg.Provision(ctx, naming.Resource{Project: shopProject, Claim: "db", HandOver: "kitchen-db"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Name != "kitchen-db" {
		t.Fatalf("the handed-over database is %q", instance.Name)
	}
	if got := getCluster(t, cnpg, "kitchen-db").GetLabels()[naming.LabelProject]; got != shopProject {
		t.Fatalf("the handed-over database records project %q", got)
	}
}

// A claim bound before names carried the project keeps the database it is
// bound to: renaming it would leave the data behind and hand the application
// an empty one.
func TestABoundClaimKeepsItsDatabaseAcrossReconciles(t *testing.T) {
	legacy := namedCluster("kitchen-db", "")
	cnpg := cnpgAgainstFakeCluster(t, legacy, appSecret("kitchen-db"))
	ctx := context.Background()

	first, err := cnpg.Provision(ctx, naming.Resource{Project: shopProject, Claim: "db", Unqualified: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "kitchen-db" {
		t.Fatalf("a claim bound under the old name got %q", first.Name)
	}
	// The next reconcile has the name on the claim's status.
	second, err := cnpg.Provision(ctx, naming.Resource{Project: shopProject, Claim: "db", Name: first.Name})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Name != first.Name {
		t.Fatalf("the bound database moved from %q to %q", first.ID, second.ID)
	}
}

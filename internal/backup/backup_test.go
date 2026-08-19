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

package backup

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
)

const testNamespace = "kitchen-system"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

// newClient builds a cluster with the status subresource behaving the way a
// real API server does, which is the half of a restore that is easiest to get
// wrong: a status written as part of an ordinary update is silently dropped.
func newClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithStatusSubresource(
			&kitchenv1alpha1.Kitchen{}, &kitchenv1alpha1.Project{}, &kitchenv1alpha1.Build{},
			&kitchenv1alpha1.Release{}, &kitchenv1alpha1.Environment{}, &kitchenv1alpha1.Connection{},
		).
		WithObjects(objects...).
		Build()
}

// platform is the installation the tests back up: a project, the connection it
// names, a build that has already finished, and the two secrets that make the
// difference between a platform that comes back and one that comes back mute.
func platform() []client.Object {
	return []client.Object{
		&kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: "default"},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", ClusterName: "prod"},
		},
		&kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: testNamespace},
		},
		&kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "shop",
				Namespace:  testNamespace,
				Finalizers: []string{"kitchen.bermos.dev/project"},
			},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source:   kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"},
				Registry: kitchenv1alpha1.RegistrySpec{},
			},
		},
		&kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1", Namespace: testNamespace},
			Spec:       kitchenv1alpha1.BuildSpec{ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"}},
			Status:     kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildSucceeded},
		},
		// A credential nothing else can reproduce: the whole reason the archive
		// carries secrets at all.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-token", Namespace: testNamespace},
			Data:       map[string][]byte{"api-token": []byte("cf-secret")},
		},
		// A credential for something the install creates for itself, which must
		// travel and must not be written back.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "kitchen-postgres",
				Namespace: testNamespace,
				Labels:    map[string]string{componentLabel: "postgres"},
			},
			Data: map[string][]byte{"password": []byte("old-password")},
		},
		// Neither of these is the platform's to keep.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.kitchen.v1", Namespace: testNamespace},
			Type:       helmReleaseSecretType,
			Data:       map[string][]byte{"release": []byte("...")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "manager-token", Namespace: testNamespace},
			Type:       corev1.SecretTypeServiceAccountToken,
			Data:       map[string][]byte{"token": []byte("...")},
		},
	}
}

// accounts is a stand-in for the identity provider's database.
type accounts struct {
	dump     accountsdb.Dump
	restored *accountsdb.Dump
}

func (a *accounts) Database() string { return a.dump.Database }

func (a *accounts) Dump(context.Context) (accountsdb.Dump, error) { return a.dump, nil }

func (a *accounts) Restore(_ context.Context, dump accountsdb.Dump) error {
	a.restored = &dump
	return nil
}

func fixtureAccounts() *accounts {
	return &accounts{dump: accountsdb.Dump{
		Database: "kitchen",
		Tables: []accountsdb.Table{
			{Name: "user", Columns: []string{"id", "email"}, Rows: 2,
				Data: []byte("1\tada@example.com\n2\tgrace@example.com\n")},
			{Name: "session", Columns: []string{"id", "userId"}, Rows: 1, Data: []byte("s1\t1\n")},
		},
	}}
}

func export(t *testing.T, from client.Client, source AccountsSource) ([]byte, Manifest) {
	t.Helper()
	exporter := &Exporter{
		Client:      from,
		Namespace:   testNamespace,
		Version:     "0.9.0",
		ClusterName: "prod",
		BaseDomain:  "apps.example.com",
		Accounts:    source,
	}
	archive := &bytes.Buffer{}
	manifest, err := exporter.WriteTo(context.Background(), archive)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return archive.Bytes(), manifest
}

// The acceptance criterion for the whole feature: an archive taken from one
// platform, restored into an empty one, brings the projects back.
func TestBackupAndRestoreIntoAnEmptyCluster(t *testing.T) {
	data, manifest := export(t, newClient(t, platform()...), fixtureAccounts())

	if manifest.Resources["projects"] != 1 || manifest.Resources["connections"] != 1 {
		t.Errorf("manifest counted %v", manifest.Resources)
	}
	if manifest.Accounts == nil || manifest.Accounts.Rows != 3 {
		t.Errorf("manifest's accounts summary: %+v", manifest.Accounts)
	}
	// Helm's release record and the service account token are the cluster's,
	// not the platform's; the other two are the platform's.
	if manifest.Secrets != 2 {
		t.Errorf("archived %d secrets, want the 2 that are the platform's", manifest.Secrets)
	}
	if len(manifest.Excluded) == 0 {
		t.Error("the manifest does not say what it leaves out")
	}

	archive, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	empty := newClient(t)
	sink := fixtureAccounts()
	restorer := &Restorer{Client: empty, Namespace: testNamespace, Version: "0.9.0", Accounts: sink}
	report, err := restorer.Restore(context.Background(), archive)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	ctx := context.Background()
	project := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Namespace: testNamespace, Name: "shop"}
	if err := empty.Get(ctx, key, project); err != nil {
		t.Fatalf("the project did not come back: %v", err)
	}
	if project.Spec.Source.Repo != "acme/shop" {
		t.Errorf("the project came back as %+v", project.Spec)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := empty.Get(ctx, types.NamespacedName{Name: "default"}, kitchen); err != nil {
		t.Fatalf("the singleton did not come back: %v", err)
	}
	if kitchen.Spec.BaseDomain != "apps.example.com" {
		t.Errorf("the singleton came back with base domain %q", kitchen.Spec.BaseDomain)
	}

	secret := &corev1.Secret{}
	if err := empty.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "cloudflare-token"}, secret); err != nil {
		t.Fatalf("the Cloudflare token did not come back: %v", err)
	}
	if string(secret.Data["api-token"]) != "cf-secret" {
		t.Errorf("the token came back as %q", secret.Data["api-token"])
	}

	if sink.restored == nil || sink.restored.Rows() != 3 {
		t.Errorf("the accounts database was not restored: %+v", sink.restored)
	}
	if report.AccountsRows != 3 {
		t.Errorf("the report says %d accounts rows", report.AccountsRows)
	}
	if report.Created["projects"] != 1 {
		t.Errorf("the report says %v", report.Created)
	}
}

// A build that comes back without its status is a build the reconciler has
// never seen, and it starts it. Restoring the history of a platform must not
// rebuild it.
func TestRestoreKeepsATerminalBuildTerminal(t *testing.T) {
	data, _ := export(t, newClient(t, platform()...), nil)
	archive, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	empty := newClient(t)
	restorer := &Restorer{Client: empty, Namespace: testNamespace, Version: "0.9.0"}
	report, err := restorer.Restore(context.Background(), archive)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(report.Warnings) > 0 {
		t.Errorf("restore warned: %v", report.Warnings)
	}

	build := &kitchenv1alpha1.Build{}
	key := types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-1"}
	if err := empty.Get(context.Background(), key, build); err != nil {
		t.Fatalf("the build did not come back: %v", err)
	}
	if build.Status.Phase != kitchenv1alpha1.BuildSucceeded {
		t.Errorf("the build came back in phase %q, so the reconciler would run it again", build.Status.Phase)
	}
}

// The credential for a database the install has just created belongs to that
// database. Writing the old one back leaves the identity provider holding a
// password its own Postgres has never heard of.
func TestRestoreLeavesFreshlyProvisionedCredentialsAlone(t *testing.T) {
	data, _ := export(t, newClient(t, platform()...), nil)
	archive, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// The cluster the archive lands in: a fresh install, with a fresh password.
	fresh := newClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-postgres",
			Namespace: testNamespace,
			Labels:    map[string]string{componentLabel: "postgres"},
		},
		Data: map[string][]byte{"password": []byte("new-password")},
	})
	restorer := &Restorer{Client: fresh, Namespace: testNamespace, Version: "0.9.0"}
	report, err := restorer.Restore(context.Background(), archive)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: testNamespace, Name: "kitchen-postgres"}
	if err := fresh.Get(context.Background(), key, secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["password"]) != "new-password" {
		t.Error("the restore overwrote the password of the database this install created")
	}
	if len(report.SecretsSkipped) != 1 || !strings.HasPrefix(report.SecretsSkipped[0], "kitchen-postgres:") {
		t.Errorf("the report does not say the secret was left alone: %v", report.SecretsSkipped)
	}
	// It is still in the archive: evidence is not the same thing as a restore
	// step, and an operator digging an old registry volume out needs it.
	var found bool
	for _, secret := range archive.Secrets {
		if secret.Name == "kitchen-postgres" && string(secret.Data["password"]) == "old-password" {
			found = true
		}
	}
	if !found {
		t.Error("the archive does not carry the old credential at all")
	}
}

// A restore over a live platform is an update, not a second create.
func TestRestoreIsIdempotent(t *testing.T) {
	data, _ := export(t, newClient(t, platform()...), nil)
	archive, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	cluster := newClient(t)
	restorer := &Restorer{Client: cluster, Namespace: testNamespace, Version: "0.9.0"}
	if _, err := restorer.Restore(context.Background(), archive); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	report, err := restorer.Restore(context.Background(), archive)
	if err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if len(report.Created) != 0 {
		t.Errorf("the second restore created %v", report.Created)
	}
	if report.Updated["projects"] != 1 {
		t.Errorf("the second restore updated %v", report.Updated)
	}
}

// The accounts dump carries rows and not a schema, and the schema is the one
// the identity provider of a particular release migrates into place. Restoring
// across releases has to be asked for.
func TestRestoreRefusesAnArchiveFromAnotherRelease(t *testing.T) {
	data, _ := export(t, newClient(t, platform()...), fixtureAccounts())
	archive, err := Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	restorer := &Restorer{Client: newClient(t), Namespace: testNamespace, Version: "0.10.0"}
	_, err = restorer.Restore(context.Background(), archive)
	if err == nil {
		t.Fatal("a restore across releases was accepted")
	}
	if !strings.Contains(err.Error(), "0.9.0") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("the refusal does not say which release wrote it, or how to override: %v", err)
	}

	restorer.Force = true
	if _, err := restorer.Restore(context.Background(), archive); err != nil {
		t.Fatalf("--force did not restore: %v", err)
	}
}

// An installation with no identity provider has no accounts to take, which is
// not a fault — and a database that could not be reached is. The archive has to
// tell the two apart, because the difference is only visible at restore time.
func TestAnArchiveWithoutAccountsSaysWhy(t *testing.T) {
	exporter := &Exporter{
		Client:          newClient(t, platform()...),
		Namespace:       testNamespace,
		Version:         "0.9.0",
		AccountsMessage: "the accounts database refused the connection",
	}
	buffer := &bytes.Buffer{}
	manifest, err := exporter.WriteTo(context.Background(), buffer)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Accounts != nil {
		t.Error("an archive with no accounts reported some")
	}
	if !strings.Contains(manifest.AccountsMessage, "refused the connection") {
		t.Errorf("the manifest does not say why: %q", manifest.AccountsMessage)
	}

	archive, err := Read(bytes.NewReader(buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	restorer := &Restorer{Client: newClient(t), Namespace: testNamespace, Version: "0.9.0", Accounts: fixtureAccounts()}
	report, err := restorer.Restore(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.AccountsMessage, "refused the connection") {
		t.Errorf("the restore did not carry the reason through: %q", report.AccountsMessage)
	}
}

// Anything that is not an archive has to be refused as one, rather than
// producing a restore that puts nothing back and reports success.
func TestReadRefusesSomethingElse(t *testing.T) {
	if _, err := Read(strings.NewReader("not a tarball")); err == nil {
		t.Fatal("a text file was accepted as an archive")
	}
	// A well-formed archive of the wrong thing: gzipped tar, no manifest.
	empty := &bytes.Buffer{}
	exporter := &Exporter{Client: newClient(t), Namespace: testNamespace, Version: "0.9.0"}
	if _, err := exporter.WriteTo(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(empty.Bytes())); err != nil {
		t.Fatalf("an archive of an empty platform is still an archive: %v", err)
	}
}

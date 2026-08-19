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
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
)

// AccountsSource is the identity provider's database, as an export needs it.
// It is an interface so that the API can hand in a live connection and a test
// can hand in a fixture — and so that an installation with no identity
// provider hands in nothing at all.
type AccountsSource interface {
	Database() string
	Dump(ctx context.Context) (accountsdb.Dump, error)
}

// Exporter writes one archive.
type Exporter struct {
	// Client reads the objects. It is a Reader because an export writes
	// nothing: the archive is the only thing this produces.
	Client client.Reader

	// Namespace is the platform namespace, which is where the namespaced
	// custom resources and every Secret worth keeping live.
	Namespace string

	// Version is the release the platform is running, recorded in the
	// manifest and checked on the way back in.
	Version string

	// ClusterName and BaseDomain identify the installation in the manifest.
	ClusterName string
	BaseDomain  string

	// Accounts is the identity provider's database. Nil writes an archive with
	// no accounts in it and says so in the manifest, which is the honest
	// answer for an installation that has no identity provider — and, when the
	// caller could not reach one it does have, AccountsMessage is what carries
	// the reason.
	Accounts AccountsSource

	// AccountsMessage explains a nil Accounts.
	AccountsMessage string
}

// WriteTo streams the archive and answers the manifest it wrote.
//
// Everything is collected before anything is written. The manifest is the
// first entry in the tar and it counts what follows it, and a reader that has
// to seek back to find out what it is holding is a reader nobody can use from
// a pipe.
func (e *Exporter) WriteTo(ctx context.Context, w io.Writer) (Manifest, error) {
	manifest := Manifest{
		Format:          Format,
		CreatedAt:       time.Now().UTC(),
		PlatformVersion: e.Version,
		ClusterName:     e.ClusterName,
		BaseDomain:      e.BaseDomain,
		Namespace:       e.Namespace,
		Resources:       map[string]int{},
		AccountsMessage: e.AccountsMessage,
		Excluded:        Excluded,
	}

	files := make([]file, 0, 64)
	for _, kind := range Kinds {
		objects, err := e.list(ctx, kind)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Resources[kind.Plural] = len(objects)
		for _, object := range objects {
			encoded, err := json.MarshalIndent(object.Object, "", "  ")
			if err != nil {
				return Manifest{}, err
			}
			files = append(files, file{
				name: fmt.Sprintf("%s%s/%s.json", ResourcesDir, kind.Plural, object.GetName()),
				data: encoded,
			})
		}
	}

	secrets, err := e.secrets(ctx)
	if err != nil {
		return Manifest{}, err
	}
	manifest.Secrets = len(secrets)
	for i := range secrets {
		encoded, err := json.MarshalIndent(&secrets[i], "", "  ")
		if err != nil {
			return Manifest{}, err
		}
		files = append(files, file{name: SecretsDir + secrets[i].Name + ".json", data: encoded})
	}

	if e.Accounts != nil {
		dump, err := e.Accounts.Dump(ctx)
		if err != nil {
			return Manifest{}, fmt.Errorf("the identity provider's database could not be dumped: %w", err)
		}
		manifest.Accounts = &AccountsSummary{
			Database: dump.Database,
			Tables:   len(dump.Tables),
			Rows:     dump.Rows(),
		}
		manifest.AccountsMessage = ""
		listing, err := json.MarshalIndent(dump, "", "  ")
		if err != nil {
			return Manifest{}, err
		}
		files = append(files, file{name: AccountsManifestPath, data: listing})
		for _, table := range dump.Tables {
			files = append(files, file{name: AccountsDir + table.Name + ".copy", data: table.Data})
		}
	}

	encodedManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, err
	}

	gzipped := gzip.NewWriter(w)
	archive := tar.NewWriter(gzipped)
	if err := writeFile(archive, manifest.CreatedAt, file{name: ManifestPath, data: encodedManifest}); err != nil {
		return Manifest{}, err
	}
	for _, entry := range files {
		if err := writeFile(archive, manifest.CreatedAt, entry); err != nil {
			return Manifest{}, err
		}
	}
	if err := archive.Close(); err != nil {
		return Manifest{}, err
	}
	if err := gzipped.Close(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// file is one entry, held until the manifest that counts it has been written.
type file struct {
	name string
	data []byte
}

func writeFile(archive *tar.Writer, modified time.Time, entry file) error {
	header := &tar.Header{
		Name:    entry.name,
		Mode:    0o600,
		Size:    int64(len(entry.data)),
		ModTime: modified,
	}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	_, err := archive.Write(entry.data)
	return err
}

// list reads every object of one kind, cleaned of the fields that belong to
// the cluster it was read from.
func (e *Exporter) list(ctx context.Context, kind Kind) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kitchenv1alpha1.GroupVersion.WithKind(kind.Kind + "List"))

	var options []client.ListOption
	if !kind.ClusterScoped {
		options = append(options, client.InNamespace(e.Namespace))
	}
	if err := e.Client.List(ctx, list, options...); err != nil {
		return nil, fmt.Errorf("cannot read the platform's %s: %w", kind.Plural, err)
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	for i := range items {
		clean(&items[i])
	}
	return items, nil
}

// secrets reads the platform namespace's Secrets.
//
// Everything in the namespace travels, minus the two kinds that are about the
// cluster rather than about the platform: a ServiceAccount's token, which the
// API server mints and the restored cluster will mint again, and Helm's own
// release records, which describe a release that the restore is not
// re-creating and which would be actively confusing if it did.
func (e *Exporter) secrets(ctx context.Context) ([]corev1.Secret, error) {
	list := &corev1.SecretList{}
	if err := e.Client.List(ctx, list, client.InNamespace(e.Namespace)); err != nil {
		return nil, fmt.Errorf("cannot read the platform's secrets: %w", err)
	}
	kept := make([]corev1.Secret, 0, len(list.Items))
	for i := range list.Items {
		secret := list.Items[i]
		if SkipSecret(&secret) {
			continue
		}
		cleanSecret(&secret)
		kept = append(kept, secret)
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Name < kept[j].Name })
	return kept, nil
}

// helmReleaseSecretType is what Helm stores a release under. It is spelled
// here rather than imported: pulling Helm's Go module in to name one string
// would tie this build to Helm's release cadence for nothing.
const helmReleaseSecretType = corev1.SecretType("helm.sh/release.v1")

// SkipSecret reports a Secret that belongs to the cluster rather than to the
// platform, and so is left out of the archive. It is exported so that the
// dashboard's count of what an export would carry is this rule and not a
// second copy of it.
func SkipSecret(secret *corev1.Secret) bool {
	switch secret.Type {
	case corev1.SecretTypeServiceAccountToken, helmReleaseSecretType:
		return true
	default:
		return false
	}
}

// clean strips what belongs to the cluster the object was read from rather
// than to the object.
//
// ownerReferences are the one that would do real damage if it were left: they
// carry the UID of an owner that will have a different one after a restore, and
// Kubernetes' garbage collector deletes an object whose owner reference points
// at a UID that does not exist. A restored archive that kept them would empty
// itself out within seconds, and look like it had worked.
func clean(object *unstructured.Unstructured) {
	metadata, ok := object.Object["metadata"].(map[string]any)
	if !ok {
		return
	}
	for _, field := range []string{
		"resourceVersion", "uid", "generation", "creationTimestamp", "managedFields",
		"selfLink", "ownerReferences", "deletionTimestamp", "deletionGracePeriodSeconds",
	} {
		delete(metadata, field)
	}
}

func cleanSecret(secret *corev1.Secret) {
	secret.APIVersion, secret.Kind = "v1", "Secret"
	secret.ResourceVersion = ""
	secret.UID = ""
	secret.Generation = 0
	secret.CreationTimestamp = metav1.Time{}
	secret.ManagedFields = nil
	secret.OwnerReferences = nil
	secret.DeletionTimestamp = nil
	secret.DeletionGracePeriodSeconds = nil
}

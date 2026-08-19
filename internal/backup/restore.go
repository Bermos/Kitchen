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
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
)

// AccountsSink is the identity provider's database, as a restore needs it.
type AccountsSink interface {
	Restore(ctx context.Context, dump accountsdb.Dump) error
}

// Archive is a read archive, held in memory.
//
// It is held rather than streamed because tar is sequential and a restore is
// not: the accounts dump's table listing arrives after the tables it describes,
// and the objects have to be applied in an order the archive does not store
// them in. Everything in here is configuration and accounts — never telemetry,
// never images — so it is small enough to hold, and maxArchiveBytes is what
// says so out loud.
type Archive struct {
	Manifest Manifest

	// Resources are the custom resources, keyed by the plural directory they
	// were found under.
	Resources map[string][]*unstructured.Unstructured

	// Secrets are the platform namespace's.
	Secrets []corev1.Secret

	// Accounts is the identity provider's database, when the archive carries
	// one.
	Accounts *accountsdb.Dump
}

// Read parses an archive.
func Read(r io.Reader) (*Archive, error) {
	gzipped, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("this is not a Kitchen backup archive: %w", err)
	}
	defer func() { _ = gzipped.Close() }()
	// The bound is on what comes *out* of the decompressor, not on what goes
	// in: a few kilobytes of gzip can expand to gigabytes, and this reads the
	// whole archive into memory. It is a real limit rather than a truncation,
	// so an archive that hits it says so instead of being reported as corrupt.
	bounded := &boundedReader{reader: gzipped, left: maxArchiveBytes}

	archive := &Archive{Resources: map[string][]*unstructured.Unstructured{}}
	tables := map[string][]byte{}
	var listing *accountsdb.Dump
	seenManifest := false

	reader := tar.NewReader(bounded)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("the archive is truncated or corrupt: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("the archive is truncated or corrupt: %w", err)
		}

		switch {
		case header.Name == ManifestPath:
			if err := json.Unmarshal(data, &archive.Manifest); err != nil {
				return nil, fmt.Errorf("the archive's manifest is unreadable: %w", err)
			}
			seenManifest = true
		case header.Name == AccountsManifestPath:
			dump := &accountsdb.Dump{}
			if err := json.Unmarshal(data, dump); err != nil {
				return nil, fmt.Errorf("the archive's accounts listing is unreadable: %w", err)
			}
			listing = dump
		case strings.HasPrefix(header.Name, AccountsDir) && strings.HasSuffix(header.Name, ".copy"):
			tables[strings.TrimSuffix(path.Base(header.Name), ".copy")] = data
		case strings.HasPrefix(header.Name, SecretsDir):
			secret := corev1.Secret{}
			if err := json.Unmarshal(data, &secret); err != nil {
				return nil, fmt.Errorf("the secret %s is unreadable: %w", header.Name, err)
			}
			archive.Secrets = append(archive.Secrets, secret)
		case strings.HasPrefix(header.Name, ResourcesDir):
			plural := path.Base(path.Dir(header.Name))
			object := &unstructured.Unstructured{}
			if err := object.UnmarshalJSON(data); err != nil {
				return nil, fmt.Errorf("the object %s is unreadable: %w", header.Name, err)
			}
			archive.Resources[plural] = append(archive.Resources[plural], object)
		}
	}

	if !seenManifest {
		return nil, errors.New("this archive has no manifest, so it is not a Kitchen backup")
	}
	if archive.Manifest.Format != Format {
		return nil, fmt.Errorf("this archive is in format %d and this release reads format %d",
			archive.Manifest.Format, Format)
	}
	if listing != nil {
		for i := range listing.Tables {
			listing.Tables[i].Data = tables[listing.Tables[i].Name]
		}
		archive.Accounts = listing
	}
	for plural := range archive.Resources {
		objects := archive.Resources[plural]
		sort.Slice(objects, func(i, j int) bool { return objects[i].GetName() < objects[j].GetName() })
	}
	return archive, nil
}

// ReadManifest reads an archive's manifest and stops there.
//
// The manifest is the first entry in the tar precisely so this is cheap: it
// answers "what is this archive, and what does it leave out" without unpacking
// a file full of credentials. It is what `kitchen backup` reports after
// writing one, and it is the same answer `tar xzOf backup.tar.gz manifest.json`
// gives somebody with no Kitchen to hand.
func ReadManifest(r io.Reader) (Manifest, error) {
	gzipped, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, fmt.Errorf("this is not a Kitchen backup archive: %w", err)
	}
	defer func() { _ = gzipped.Close() }()

	reader := tar.NewReader(&boundedReader{reader: gzipped, left: maxArchiveBytes})
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return Manifest{}, errors.New("this archive has no manifest, so it is not a Kitchen backup")
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("the archive is truncated or corrupt: %w", err)
		}
		if header.Name != ManifestPath {
			continue
		}
		manifest := Manifest{}
		if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
			return Manifest{}, fmt.Errorf("the archive's manifest is unreadable: %w", err)
		}
		return manifest, nil
	}
}

// errArchiveTooLarge is what an archive past maxArchiveBytes reads as. An
// archive is the platform's configuration and its accounts — never its
// telemetry, never its images — so anything this size is a mistake or an
// attack, and either way it is not something to unpack into memory.
var errArchiveTooLarge = fmt.Errorf(
	"this archive unpacks to more than %d bytes, which is far past any platform's configuration and accounts",
	int64(maxArchiveBytes))

// boundedReader stops at a byte count with an error of its own, where an
// io.LimitReader would stop with an EOF that reads as a truncated tar.
type boundedReader struct {
	reader io.Reader
	left   int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errArchiveTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.reader.Read(p)
	b.left -= int64(n)
	return n, err
}

// Restorer applies an archive to a cluster.
//
// It is deliberately not a reconciler and not an API handler: restoring is
// bootstrap, and the credentials to log into the dashboard are *inside* the
// archive. An installation whose accounts database is gone has nobody who can
// press a button, which is why this runs as a Job with its own grant instead —
// the same category as the install itself and the bootstrap link (see
// CLAUDE.md, "Nothing needs kubectl").
type Restorer struct {
	Client    client.Client
	Namespace string

	// Version is the release doing the restoring. An archive from another one
	// is refused unless Force says otherwise: the accounts half is a data-only
	// dump into a schema the identity provider migrates for itself, so the two
	// releases have to agree about what that schema looks like.
	Version string
	Force   bool

	// Accounts is where the identity provider's data goes. Nil restores the
	// cluster half alone and says so in the report.
	Accounts AccountsSink
}

// Report is what a restore did.
type Report struct {
	// Created and Updated count objects by plural name. An object that was
	// already there is updated rather than skipped: a restore is a statement
	// about what the platform should look like, not a merge.
	Created map[string]int `json:"created"`
	Updated map[string]int `json:"updated"`

	// Secrets restored, and the ones deliberately left alone.
	Secrets        int      `json:"secrets"`
	SecretsSkipped []string `json:"secretsSkipped,omitempty"`

	// AccountsRows is how many rows went back into the identity provider's
	// database, and AccountsMessage explains a restore that put none there.
	AccountsRows    int64  `json:"accountsRows"`
	AccountsMessage string `json:"accountsMessage,omitempty"`

	// Warnings are the things that did not work and did not stop the restore:
	// a status subresource that would not take, most often. They are reported
	// rather than swallowed, because a restore nobody was told was partial is
	// worse than one that failed.
	Warnings []string `json:"warnings,omitempty"`
}

// Restore applies the archive. It does not delete anything: an object that is
// in the cluster and not in the archive is left where it is, because a restore
// that pruned would be a restore that could not be run twice.
func (r *Restorer) Restore(ctx context.Context, archive *Archive) (Report, error) {
	if err := r.checkVersion(archive.Manifest); err != nil {
		return Report{}, err
	}
	report := Report{Created: map[string]int{}, Updated: map[string]int{}}

	for _, secret := range archive.Secrets {
		if reason, skip := skipOnRestore(&secret); skip {
			report.SecretsSkipped = append(report.SecretsSkipped, secret.Name+": "+reason)
			continue
		}
		if err := r.applySecret(ctx, secret); err != nil {
			return report, err
		}
		report.Secrets++
	}

	for _, kind := range Kinds {
		for _, object := range archive.Resources[kind.Plural] {
			created, warning, err := r.applyObject(ctx, kind, object)
			if err != nil {
				return report, err
			}
			if warning != "" {
				report.Warnings = append(report.Warnings, warning)
			}
			if created {
				report.Created[kind.Plural]++
			} else {
				report.Updated[kind.Plural]++
			}
		}
	}

	switch {
	case archive.Accounts == nil:
		report.AccountsMessage = "this archive carries no accounts data: " + archive.Manifest.AccountsMessage
	case r.Accounts == nil:
		report.AccountsMessage = "the identity provider's database was not restored, because this restore was " +
			"given no connection to one"
	default:
		if err := r.Accounts.Restore(ctx, *archive.Accounts); err != nil {
			return report, err
		}
		report.AccountsRows = archive.Accounts.Rows()
	}
	return report, nil
}

// checkVersion refuses an archive from a release this one cannot vouch for.
func (r *Restorer) checkVersion(manifest Manifest) error {
	if r.Force || r.Version == "" || manifest.PlatformVersion == "" || manifest.PlatformVersion == r.Version {
		return nil
	}
	return fmt.Errorf("this archive was written by Kitchen %s and this is %s. Restore it with the release that "+
		"wrote it and upgrade afterwards, or pass --force to restore anyway — the accounts dump carries rows and "+
		"not a schema, so it only fits the schema its own release migrates into place",
		manifest.PlatformVersion, r.Version)
}

// infrastructureComponents are the chart components whose Secret is a
// credential for something the install has just created from scratch, rather
// than a credential for something outside the cluster.
//
// Restoring one of these is actively harmful: a freshly installed Postgres has
// the password the chart generated for it, and writing the old one back into
// the Secret leaves the identity provider holding a password its own database
// has never heard of. They travel in the archive anyway — an operator
// recovering images out of an old registry volume needs the old registry
// password, and evidence is not the same thing as a restore step — and they
// are named in the report so nobody has to guess which ones were left.
var infrastructureComponents = map[string]string{
	"postgres": "the accounts database's own password belongs to the database this install created, " +
		"not to the one that was lost; its *contents* are restored instead",
	"clickhouse": "the telemetry store's password belongs to the store this install created, and telemetry " +
		"is not restored at all",
	"registry": "the bundled registry's password is the one it was installed with, and the seeded " +
		"Connection is kept in step with it by the operator",
}

// componentLabel is what the chart marks each of its components with.
const componentLabel = "app.kubernetes.io/component"

func skipOnRestore(secret *corev1.Secret) (string, bool) {
	reason, found := infrastructureComponents[secret.Labels[componentLabel]]
	return reason, found
}

// applySecret writes one Secret back.
//
// A Secret that is already there keeps everything but its data. The chart owns
// several of these, and Helm decides what it owns by the labels and annotations
// on the object: replacing those with the archive's would leave the release
// unable to upgrade itself, over a difference that does not matter. What
// matters is the bytes, and the bytes are what is replaced.
func (r *Restorer) applySecret(ctx context.Context, secret corev1.Secret) error {
	secret.Namespace = r.Namespace
	existing := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: secret.Name}
	switch err := r.Client.Get(ctx, key, existing); {
	case apierrors.IsNotFound(err):
		if err := r.Client.Create(ctx, &secret); err != nil {
			return fmt.Errorf("cannot restore the secret %s: %w", secret.Name, err)
		}
		return nil
	case err != nil:
		return err
	}
	existing.Data = secret.Data
	existing.StringData = nil
	if err := r.Client.Update(ctx, existing); err != nil {
		return fmt.Errorf("cannot restore the secret %s: %w", secret.Name, err)
	}
	return nil
}

// applyObject writes one custom resource back, status included.
//
// The status matters more than it looks. A Build restored without one is a
// Build the reconciler has never seen, and it starts it: a restore would then
// re-run every build in the archive's history against a registry it may no
// longer have credentials for. The same reasoning covers Releases and
// Environments, whose reconcilers converge on what the spec asks for either
// way but would spend a while reporting the platform as broken while they did.
func (r *Restorer) applyObject(
	ctx context.Context,
	kind Kind,
	object *unstructured.Unstructured,
) (created bool, warning string, err error) {
	desired := object.DeepCopy()
	desired.SetGroupVersionKind(kitchenv1alpha1.GroupVersion.WithKind(kind.Kind))
	if !kind.ClusterScoped {
		desired.SetNamespace(r.Namespace)
	}
	status, hasStatus := desired.Object["status"]
	delete(desired.Object, "status")

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	key := types.NamespacedName{Namespace: desired.GetNamespace(), Name: desired.GetName()}

	switch getErr := r.Client.Get(ctx, key, existing); {
	case apierrors.IsNotFound(getErr):
		if err := r.Client.Create(ctx, desired); err != nil {
			return false, "", fmt.Errorf("cannot restore the %s %s: %w", kind.Kind, desired.GetName(), err)
		}
		created = true
	case getErr != nil:
		return false, "", getErr
	default:
		// Everything the archive knows about, onto the object that is there.
		// The resourceVersion is the live one's — the archive's was stripped
		// on the way out — so this is an ordinary optimistic update and not a
		// force.
		merged := desired.DeepCopy()
		merged.SetResourceVersion(existing.GetResourceVersion())
		merged.SetUID(existing.GetUID())
		if err := r.Client.Update(ctx, merged); err != nil {
			return false, "", fmt.Errorf("cannot restore the %s %s: %w", kind.Kind, desired.GetName(), err)
		}
		desired = merged
	}

	if !hasStatus {
		return created, "", nil
	}
	// Read the object back before writing its status: a create answers with
	// the object the API server stored, but an update through the typed client
	// leaves the caller's copy holding the resourceVersion from before the
	// write on some paths, and a status update on a stale one conflicts.
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(desired.GroupVersionKind())
	if err := r.Client.Get(ctx, key, current); err != nil {
		return created, "", err
	}
	current.Object["status"] = status
	if err := r.Client.Status().Update(ctx, current); err != nil {
		// A status that would not take is worth saying and not worth failing
		// on: the object is restored, and its reconciler writes a status of
		// its own within a reconcile or two.
		return created, fmt.Sprintf("the %s %s was restored but its status was not: %s",
			kind.Kind, desired.GetName(), err), nil
	}
	return created, "", nil
}

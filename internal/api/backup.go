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

package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/accountsdb"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The backup endpoints: what an export would carry, and the export itself.
//
// There is no restore endpoint, and its absence is the design rather than a
// gap. Restoring happens into a cluster whose accounts database is gone, and
// the credentials to log into this API are inside the archive — so there is
// nobody left to authenticate. Restore is a Job with a grant of its own, which
// puts it in the same category as installing the chart and following the
// bootstrap link: cluster bootstrap, the exception CLAUDE.md keeps to "nothing
// needs kubectl". See docs/BACKUP.md.
//
// The export is a POST rather than a GET, for two reasons that point the same
// way. The body is every credential the platform holds, and a GET is the verb
// browsers, proxies and histories treat as safe to repeat and worth caching.
// And it is a request the audit log has to carry: somebody took a copy of
// everything, and that is exactly the sentence an audit log exists to be able
// to produce afterwards.

// Whether this cluster can snapshot a volume is a read of the snapshot
// controller's own classes. Kitchen neither installs nor requires it, and a
// grant for a group that is not registered is simply a grant nothing uses — a
// list of a kind the cluster does not have is refused by discovery, not by
// authorization, which is what the Backup screen reports.
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch

// backupTimeout bounds an export. It reads every custom resource, every secret
// in the platform namespace and the whole accounts database; a minute is far
// past what any of that takes, and is there so that an unreachable database
// costs a request rather than a connection held open until the client gives up.
const backupTimeout = time.Minute

// snapshotClassGVK is the CSI snapshot controller's class kind. Kitchen does
// not install it and does not depend on it — see docs/BACKUP.md, "PVC
// snapshots" — but where a cluster has one, it is the cheap answer for the
// two volumes this archive cannot carry, so the screen says whether it works.
var snapshotClassGVK = schema.GroupVersionKind{
	Group:   "snapshot.storage.k8s.io",
	Version: "v1",
	Kind:    "VolumeSnapshotClassList",
}

// backupView is what the Backup screen shows before anybody presses anything:
// what an archive taken now would hold, what it would not, and whether the two
// halves the platform cannot carry have a snapshot controller behind them.
type backupView struct {
	// PlatformVersion is the release an archive would be written by, and so
	// the release it can be restored into.
	PlatformVersion string `json:"platformVersion"`
	ClusterName     string `json:"clusterName,omitempty"`
	BaseDomain      string `json:"baseDomain,omitempty"`

	// Resources is how many objects of each kind an export would carry, keyed
	// by plural name, and Secrets how many secrets would travel with them.
	Resources map[string]int `json:"resources"`
	Secrets   int            `json:"secrets"`

	// Accounts is the identity provider's database: whether it can be reached
	// and what it is called. An installation with no identity provider has
	// none, which is not a fault; one that cannot be reached is.
	Accounts backupAccountsView `json:"accounts"`

	// Excluded is what an archive deliberately leaves out. It is served rather
	// than written into the dashboard, so that the screen and the archive's own
	// manifest cannot come to disagree.
	Excluded []string `json:"excluded"`

	// Snapshots is whether volume snapshots are an option on this cluster.
	Snapshots snapshotSupportView `json:"snapshots"`

	// Schedule is the scheduled backup: when it runs, where it writes, and —
	// the part worth reading first — when one last worked. A screen that only
	// showed what an archive *would* carry could not tell an operator that
	// the last one was taken in March.
	Schedule backupScheduleView `json:"schedule"`

	// Filename is what the download should be called, so that an archive on
	// somebody's disk still says which platform and which day it came from.
	Filename string `json:"filename"`
}

type backupAccountsView struct {
	// Available is whether an export taken now would carry the accounts.
	Available bool   `json:"available"`
	Database  string `json:"database,omitempty"`
	// Message explains an unavailable database, and is empty otherwise.
	Message string `json:"message,omitempty"`
}

// snapshotSupportView answers the question issue #64 turned up: a cluster can
// have a snapshot controller running and no CRDs for it to act on, in which
// case a VolumeSnapshot is accepted by nothing and nobody is told. So this
// reports what is actually installed rather than assuming a CSI driver
// implies snapshots.
type snapshotSupportView struct {
	// Supported is true only when the API is registered *and* a class exists
	// to snapshot into. Either half missing is a snapshot that never happens.
	Supported bool `json:"supported"`
	// Classes are the VolumeSnapshotClasses that could be used.
	Classes []string `json:"classes,omitempty"`
	// Message says what is missing, in the words that name the fix.
	Message string `json:"message,omitempty"`
}

// getBackup describes what an export would carry.
func (s *Server) getBackup(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	view := backupView{
		PlatformVersion: s.Version,
		ClusterName:     kitchen.Spec.ClusterName,
		BaseDomain:      kitchen.Spec.BaseDomain,
		Resources:       map[string]int{},
		Excluded:        backup.Excluded,
		Filename:        backupFilename(kitchen, time.Now().UTC()),
		Snapshots:       s.snapshotSupport(ctx),
		Schedule:        newBackupScheduleView(kitchen),
	}
	for _, kind := range backup.Kinds {
		count, err := s.countKind(ctx, kind)
		if err != nil {
			s.writeError(w, err)
			return
		}
		view.Resources[kind.Plural] = count
	}

	secrets := &corev1.SecretList{}
	if err := s.reader().List(ctx, secrets, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	for i := range secrets.Items {
		if !backupCarriesSecret(&secrets.Items[i]) {
			continue
		}
		view.Secrets++
	}

	accounts, message := s.accountsDatabase(ctx, kitchen)
	if accounts != nil {
		view.Accounts = backupAccountsView{Available: true, Database: accounts.Database()}
		accounts.Close(ctx)
	} else {
		view.Accounts = backupAccountsView{Message: message}
	}
	writeJSON(w, http.StatusOK, view)
}

// createBackup streams the archive.
func (s *Server) createBackup(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), backupTimeout)
	defer cancel()

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	if !s.recorded(w, req, audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditExport,
		Reason: "the platform's state was exported: every custom resource, every secret in " +
			controller.PlatformNamespace + ", and the identity provider's database",
		Details: map[string]any{"platformVersion": s.Version},
	}) {
		return
	}

	accounts, message := s.accountsDatabase(ctx, kitchen)
	if accounts != nil {
		defer accounts.Close(ctx)
	}
	exporter := &backup.Exporter{
		// The uncached reader: an export is rare, reads every secret in the
		// namespace, and asking the manager's cache for those would mean an
		// informer over every Secret kept warm for a button nobody presses
		// most weeks.
		Client:          s.reader(),
		Namespace:       s.Namespace,
		Version:         s.Version,
		ClusterName:     kitchen.Spec.ClusterName,
		BaseDomain:      kitchen.Spec.BaseDomain,
		AccountsMessage: message,
	}
	if accounts != nil {
		exporter.Accounts = accounts
	}

	// The headers go out before the body is built, because the body is built
	// as it is written. That is also why a failure part-way cannot become a
	// JSON error: the status line has already been sent, so the archive is
	// truncated instead — and a truncated gzip stream is exactly what Read
	// refuses, which is the honest outcome. It is logged here so the operator
	// can find out why.
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", backupFilename(kitchen, time.Now().UTC())))
	w.WriteHeader(http.StatusOK)

	manifest, err := exporter.WriteTo(ctx, w)
	if err != nil {
		s.log().Error(err, "the platform backup failed part-way and the archive is truncated",
			"caller", callerName(caller))
		return
	}
	s.log().Info("platform backup exported", "caller", callerName(caller),
		"secrets", manifest.Secrets, "resources", manifest.Resources)
}

// countKind is how many objects of one kind an archive would carry.
func (s *Server) countKind(ctx context.Context, kind backup.Kind) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kitchenv1alpha1.GroupVersion.WithKind(kind.Kind + "List"))
	var options []client.ListOption
	if !kind.ClusterScoped {
		options = append(options, client.InNamespace(s.Namespace))
	}
	if err := s.Client.List(ctx, list, options...); err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

// backupCarriesSecret is the export's own rule, asked here so the screen's
// count and the archive cannot disagree.
func backupCarriesSecret(secret *corev1.Secret) bool {
	return !backup.SkipSecret(secret)
}

// accountsDatabase opens a connection to the identity provider's database, or
// says why it could not. Both halves of "could not" are real and the message
// has to tell them apart: an installation with no identity provider has no
// accounts to take, and one whose database is unreachable has accounts it is
// not backing up.
func (s *Server) accountsDatabase(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (accountsConnection, string) {
	if s.accountsDB != nil {
		return s.accountsDB(ctx, kitchen)
	}
	if !kitchen.Spec.Auth.Enabled {
		return nil, "this installation has no identity provider, so there are no accounts to back up"
	}

	name := accountsdb.SecretName(kitchen)
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("the accounts database's secret %s is not in %s, so the accounts cannot be "+
				"read. Set spec.auth.databaseSecretRef on the Kitchen object to whichever secret holds the "+
				"connection.", name, s.Namespace)
		}
		return nil, fmt.Sprintf("the accounts database's secret could not be read: %s", err)
	}
	dsn, err := accountsdb.DSNFromSecret(secret)
	if err != nil {
		return nil, err.Error()
	}
	client, err := accountsdb.Connect(ctx, dsn)
	if err != nil {
		return nil, err.Error()
	}
	return client, ""
}

// snapshotSupport reports whether this cluster could snapshot a volume.
func (s *Server) snapshotSupport(ctx context.Context) snapshotSupportView {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(snapshotClassGVK)
	// The uncached reader, deliberately: a cached list of a kind the cluster
	// has never heard of would start an informer that can only fail, and it
	// would keep failing for the life of the process.
	if err := s.reader().List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return snapshotSupportView{Message: "this cluster has no VolumeSnapshot API, so volume snapshots are " +
				"not an option here. Installing a CSI snapshot controller means installing its CRDs too — a " +
				"controller with no CRDs behind it accepts nothing and reports nothing."}
		}
		return snapshotSupportView{Message: "whether this cluster can snapshot volumes could not be determined: " +
			err.Error()}
	}
	classes := make([]string, 0, len(list.Items))
	for i := range list.Items {
		classes = append(classes, list.Items[i].GetName())
	}
	sort.Strings(classes)
	if len(classes) == 0 {
		return snapshotSupportView{Message: "the VolumeSnapshot API is registered but no VolumeSnapshotClass " +
			"exists, so a snapshot would be accepted and never taken. Install one for this cluster's CSI driver."}
	}
	return snapshotSupportView{Supported: true, Classes: classes}
}

// backupFilename names the download after the installation and the day. The
// naming itself is internal/backup's, so that an archive taken from this
// button and one taken by the schedule are indistinguishable — and so that
// retention, which deletes by that name, recognises both.
func backupFilename(kitchen *kitchenv1alpha1.Kitchen, now time.Time) string {
	return backup.Filename(kitchen.Spec.ClusterName, kitchen.Spec.BaseDomain, now)
}

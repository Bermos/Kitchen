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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// CloudNativePG: Postgres in the cluster Kitchen was installed into, which is
// the answer to a platform whose pitch is "bring your own Kubernetes" being
// unable to give you a database without a SaaS account.
//
// Its kinds are addressed as unstructured objects rather than through cnpg's
// Go types, for the reason cert-manager's are: importing an operator's module
// ties this build to its release cadence, and the four fields written here are
// not worth that.

const (
	// DefaultDatabaseNamespace is where the Clusters go. It is deliberately
	// *not* the project's application namespace: deleting a project deletes
	// that namespace, and a claim under deletionPolicy Retain must survive
	// exactly that. It is one namespace for every project's databases, which
	// is the same trust model the rest of the platform has — one team, one
	// cluster (docs/SCOPE.md).
	DefaultDatabaseNamespace = "kitchen-databases"

	// DefaultStorageSize is what a claim that names no size gets. Small
	// enough to be free on any cluster with a default StorageClass, and the
	// claim says so when it wants more.
	DefaultStorageSize = "10Gi"

	// DefaultInstances is how many Postgres instances a Cluster runs. One:
	// the platform is not going to decide on somebody's behalf that a preview
	// database deserves three nodes, and the Connection is where an
	// installation says otherwise for all of its databases at once.
	DefaultInstances = 1

	// applicationDatabase and applicationUser are what the database and its
	// owner are called inside every provisioned Cluster. They are constants
	// rather than claim fields because the binding Secret is the interface —
	// an application reads `url`, and what the role happens to be called on
	// the other side of it is not a decision anybody needs to make.
	applicationDatabase = "app"
	applicationUser     = "app"

	// managedByLabel marks the Clusters this provisioner created, the way
	// every other object the platform writes is marked. A Cluster without it
	// was somebody else's and is never written to.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kitchen"

	// clusterLabel is what cnpg puts on the pods of one Cluster; reading it
	// is how the primary's node — and so the residency — is found.
	clusterLabel = "cnpg.io/cluster"

	// hibernationAnnotation is CloudNativePG's declarative hibernation: "on"
	// stops every instance of a Cluster and leaves its PersistentVolumeClaims
	// alone, "off" starts them again from those volumes. It is how a preview's
	// database parks with its preview (#294) — a Cluster's instance count has
	// a floor of one, so hibernation is the operator's supported way of
	// saying zero.
	hibernationAnnotation = "cnpg.io/hibernation"
	hibernationOn         = "on"
	hibernationOff        = "off"

	// regionLabel and zoneLabel are the standard topology labels. Residency
	// is *reported*, never declared, so what a node happens to say about
	// itself is exactly the right source: an unlabelled node reports nothing
	// and the claim records no residency rather than a guess.
	regionLabel = "topology.kubernetes.io/region"
	zoneLabel   = "topology.kubernetes.io/zone"
)

// clusterGVK is CloudNativePG's database.
func clusterGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
}

// CNPG provisions databases as CloudNativePG Clusters in the platform's own
// cluster. It holds no credential: it writes through the operator's service
// account, which is what makes `cnpg` the one Connection provider with
// nothing in its credentials Secret.
type CNPG struct {
	// Client is the platform's cluster.
	Client client.Client
	// Namespace the Clusters are created in.
	Namespace string
	// Images the platform may run a database from, in preference order.
	Images []PostgresImage
	// StorageSize and StorageClass are the defaults a claim that names
	// neither gets.
	StorageSize  string
	StorageClass string
	// Instances per Cluster.
	Instances int
}

// cnpgConfig is the `cnpg` slice of a Connection's spec.config: the defaults
// every claim through this Connection inherits, and the catalogue of images
// the installation will actually run. Both are the operator's to set, which
// is why they are on the Connection and not on the claim — a developer asking
// for an extension should not be able to choose the image it arrives in.
type cnpgConfig struct {
	Namespace    string          `json:"namespace,omitempty"`
	StorageSize  string          `json:"storageSize,omitempty"`
	StorageClass string          `json:"storageClass,omitempty"`
	Instances    int             `json:"instances,omitempty"`
	Images       []PostgresImage `json:"images,omitempty"`

	// Backup is the backup policy every claim through this Connection
	// inherits — one decision for every database the installation
	// provisions, which a claim may then override. It is the same shape as
	// the claim's own field so that the two compose by field rather than by
	// translation.
	Backup *kitchenv1alpha1.ClaimBackupSpec `json:"backup,omitempty"`
}

// ConnectionBackupPolicy is the backup default an operator set on a database
// Connection, and nil where they set none.
//
// It lives here rather than in the reconciler because the `cnpg` slice of a
// Connection's config is this package's vocabulary: the reconciler asks what
// the Connection says, and does not have to know how the Connection spells
// it. A Connection through a provider that backs itself up answers nil, which
// is correct — there is no policy for such a claim to inherit.
func ConnectionBackupPolicy(conn *kitchenv1alpha1.Connection) *kitchenv1alpha1.ClaimBackupSpec {
	if conn == nil || conn.Spec.Provider != ProviderCNPG || conn.Spec.Config == nil ||
		len(conn.Spec.Config.Raw) == 0 {
		return nil
	}
	cfg := cnpgConfig{}
	if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
		return nil
	}
	return cfg.Backup
}

// NewCNPG builds the provisioner from a Connection.
func NewCNPG(opts Options) (*CNPG, error) {
	if opts.Cluster == nil {
		return nil, fmt.Errorf("the %s provider provisions into this cluster and was given no client to do it with",
			ProviderCNPG)
	}
	cfg := cnpgConfig{}
	if conn := opts.Connection; conn != nil && conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid %s config: %w", ProviderCNPG, err)
		}
	}

	provisioner := &CNPG{
		Client:       opts.Cluster,
		Namespace:    firstNonEmpty(cfg.Namespace, opts.Namespace, DefaultDatabaseNamespace),
		Images:       cfg.Images,
		StorageSize:  firstNonEmpty(cfg.StorageSize, DefaultStorageSize),
		StorageClass: cfg.StorageClass,
		Instances:    cfg.Instances,
	}
	if len(provisioner.Images) == 0 {
		provisioner.Images = DefaultPostgresImages
	}
	if provisioner.Instances <= 0 {
		provisioner.Instances = DefaultInstances
	}
	return provisioner, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Provision creates a database with nothing asked of it beyond a name.
func (c *CNPG) Provision(ctx context.Context, res naming.Resource) (Instance, error) {
	return c.ProvisionWith(ctx, res, Requirements{})
}

// ProvisionWith creates (or finds) the Cluster for a claim.
//
// The requirements are resolved to an image *first*, before anything is
// created and before the cluster is even read: a claim asking for an
// extension nothing supplies is refused as a claim, which is the whole reason
// this method exists rather than Provision alone.
func (c *CNPG) ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error) {
	resolution, err := resolveImage(c.Images, req)
	if err != nil {
		return Instance{}, err
	}

	name, err := naming.Resolve(ctx, res, naming.Provider{
		Kind: "database", Limit: maxClusterName, Lookup: c.owner,
	})
	if err != nil {
		return Instance{}, err
	}

	cluster, err := c.ensureCluster(ctx, name, res.Project, resolution, req, nil)
	if err != nil {
		return Instance{}, err
	}
	binding, err := c.binding(ctx, cluster.GetName())
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ID:      c.Namespace + "/" + cluster.GetName(),
		Name:    cluster.GetName(),
		Binding: binding,
		// A database the platform provisions for an environment holds that
		// environment's own data, and the primary is production's. Only a
		// preview's Cluster is synthetic, and CreateBranch is where that is
		// declared.
		Provenance: ProvenanceProduction,
		Region:     c.region(ctx, cluster.GetName()),
	}, nil
}

// CreateBranch gives a preview Environment its own empty database.
//
// There is no copy-on-write branch here and there is not going to be: cnpg's
// nearest equivalent is a pg_basebackup of the parent, which is slow, doubles
// the storage, and — the part that actually decides it — puts production data
// in a preview environment. A preview gets a fresh database with the same
// image, the same extensions and the same storage as its parent, and the
// claim's status says dataProvenance: synthetic, which is both true and the
// state an auditor wants the platform to be unable to leave.
func (c *CNPG) CreateBranch(ctx context.Context, instanceID, name string) (Branch, error) {
	parent, err := c.cluster(ctx, instanceID)
	if err != nil {
		return Branch{}, err
	}
	child, err := c.ensureCluster(ctx, branchName(parent.GetName(), name),
		parent.GetLabels()[naming.LabelProject], Resolution{}, Requirements{}, parent)
	if err != nil {
		return Branch{}, err
	}
	binding, err := c.binding(ctx, child.GetName())
	if err != nil {
		return Branch{}, err
	}
	return Branch{
		ID:         c.Namespace + "/" + child.GetName(),
		Binding:    binding,
		Provenance: ProvenanceSynthetic,
	}, nil
}

// Deprovision deletes the Cluster, and with it the volumes behind it —
// CloudNativePG garbage-collects a Cluster's PVCs when the Cluster goes. That
// is what `deletionPolicy: Delete` means for a database with a PVC behind it,
// and why Retain is the default: under Retain the Cluster is left running in
// the platform's database namespace, still costing storage, and a claim of
// the same name created later finds it again by name and rebinds to it.
func (c *CNPG) Deprovision(ctx context.Context, instanceID string) error {
	return c.deleteCluster(ctx, instanceID)
}

// DeleteBranch removes a preview's database, or a recovered sibling nobody
// asked for any more. Always, under either deletion policy: a preview's data
// is the preview's, and it goes when the preview does.
//
// The base backup schedule goes with it. A preview has none, but a recovery
// inherits its source's (see cnpg_recovery.go) — and a ScheduledBackup left
// behind naming a Cluster that is gone is a job failing nightly about a
// database nobody has.
func (c *CNPG) DeleteBranch(ctx context.Context, _, branchID string) error {
	if strings.TrimSpace(branchID) == "" {
		return nil
	}
	namespace, name := splitID(branchID, c.Namespace)
	if err := c.deleteScheduledBackup(ctx, namespace, name); err != nil {
		return err
	}
	return c.deleteCluster(ctx, branchID)
}

// IdleBranch hibernates a preview's Cluster: CloudNativePG's own declarative
// hibernation, which stops every instance and leaves the PersistentVolumeClaims
// exactly where they are. A preview that wakes finds the database it left.
//
// It is the annotation rather than `spec.instances: 0` because a Cluster's
// instance count has a floor of one — asking for zero is refused at admission,
// and asking the operator to hibernate is the supported way to say the same
// thing.
func (c *CNPG) IdleBranch(ctx context.Context, branchID string) error {
	return c.hibernate(ctx, branchID, hibernationOn)
}

// WakeBranch takes the hibernation back off. CloudNativePG starts the
// instances again from the volumes it left; this returns as soon as the
// Cluster has been asked, and the claim's own readiness path reports when it
// is serving.
func (c *CNPG) WakeBranch(ctx context.Context, branchID string) error {
	return c.hibernate(ctx, branchID, hibernationOff)
}

// hibernate writes the hibernation annotation, treating a Cluster that is
// already in the wanted state as done and one that is gone — or a cluster no
// longer serving cnpg at all — as nothing to do. A park must never be able to
// wedge a reconcile: the worst case is a preview that keeps running, which is
// what the platform did before this existed.
func (c *CNPG) hibernate(ctx context.Context, branchID, state string) error {
	if strings.TrimSpace(branchID) == "" {
		return nil
	}
	cluster, err := c.cluster(ctx, branchID)
	if err != nil {
		if apierrors.IsNotFound(err) || errors.Is(err, ErrUnsatisfiable) {
			return nil
		}
		return err
	}
	annotations := cluster.GetAnnotations()
	if annotations[hibernationAnnotation] == state {
		return nil
	}
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[hibernationAnnotation] = state
	cluster.SetAnnotations(annotations)
	if err := c.Client.Update(ctx, cluster); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	return nil
}

// ensureCluster creates the Cluster if it is not there and waits for it to be
// ready. A parent makes this a branch: the child copies the parent's image,
// storage and bootstrap SQL, so a preview runs the same Postgres with the same
// extensions — and, because the bootstrap is copied and not the *data*, an
// empty one.
func (c *CNPG) ensureCluster(
	ctx context.Context,
	name string,
	project string,
	resolution Resolution,
	req Requirements,
	parent *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(clusterGVK())
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, existing)
	switch {
	case err == nil:
		// Found. Nothing here rewrites a running database's image, its
		// extensions or its volume: a major version is not something you
		// change under a live Postgres, and a claim that asks for a different
		// one is asking for a different database. The claim's config is
		// applied when the Cluster is created and documented as such.
		//
		// The project label is the exception, and only where there is none:
		// a database an operator has just handed over records whose it is
		// from now on, so the next claim of that name is answered from the
		// record rather than from the hand-over.
		if err := c.recordProject(ctx, existing, project); err != nil {
			return nil, err
		}
		return existing, c.ready(existing)
	case meta.IsNoMatchError(err):
		return nil, notInstalled(err)
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	desired, err := c.desiredCluster(name, project, resolution, req, parent)
	if err != nil {
		return nil, err
	}
	if err := c.ensureNamespace(ctx); err != nil {
		return nil, err
	}
	if err := c.Client.Create(ctx, desired); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, notInstalled(err)
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// Two reconciles raced; the winner's object is the one to read.
		if err := c.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, desired); err != nil {
			return nil, err
		}
	}
	return desired, c.ready(desired)
}

// ensureNamespace creates the database namespace on the way to the first
// database, so that an installation never has to have made one. It is
// deliberately bare — no Pod Security relaxation — because CloudNativePG's
// own pods run unprivileged and need none, which is the difference between
// this namespace and an application one.
func (c *CNPG) ensureNamespace(ctx context.Context) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   c.Namespace,
		Labels: map[string]string{managedByLabel: managedByValue},
	}}
	err := c.Client.Create(ctx, namespace)
	if err == nil || apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// desiredCluster builds the Cluster object.
func (c *CNPG) desiredCluster(
	name string,
	project string,
	resolution Resolution,
	req Requirements,
	parent *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	image := resolution.Image
	storageSize := firstNonEmpty(req.StorageSize, c.StorageSize)
	storageClass := firstNonEmpty(req.StorageClass, c.StorageClass)
	bootstrap := initDB(resolution.Extensions)

	if parent != nil {
		// A branch inherits everything the parent was created with, so a
		// preview is the same database shape as production with none of its
		// data. Anything the parent does not have falls back to the same
		// defaults a first provision would use.
		image = firstNonEmpty(nestedString(parent, "spec", "imageName"), image)
		storageSize = firstNonEmpty(nestedString(parent, "spec", "storage", "size"), storageSize)
		storageClass = firstNonEmpty(nestedString(parent, "spec", "storage", "storageClass"), storageClass)
		if inherited, found, err := unstructured.NestedMap(parent.Object, "spec", "bootstrap"); err == nil && found {
			bootstrap = inherited
		}
	}
	if image == "" {
		resolved, err := resolveImage(c.Images, Requirements{})
		if err != nil {
			return nil, err
		}
		image = resolved.Image
	}

	storage := map[string]any{"size": firstNonEmpty(storageSize, DefaultStorageSize)}
	if storageClass != "" {
		storage["storageClass"] = storageClass
	}

	cluster := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"instances": int64(c.Instances),
			"imageName": image,
			"bootstrap": bootstrap,
			"storage":   storage,
		},
	}}
	cluster.SetGroupVersionKind(clusterGVK())
	cluster.SetName(name)
	cluster.SetNamespace(c.Namespace)
	labels := map[string]string{managedByLabel: managedByValue}
	if project != "" {
		labels[naming.LabelProject] = project
	}
	cluster.SetLabels(labels)
	return cluster, nil
}

// owner answers naming.Lookup: whether a Cluster of that name is there and
// which project it was created for. A cluster that does not serve cnpg at
// all has no databases in it, so nothing is there to adopt.
func (c *CNPG) owner(ctx context.Context, name string) (naming.Owner, error) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(clusterGVK())
	err := c.Client.Get(ctx, types.NamespacedName{Namespace: c.Namespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return naming.Owner{}, nil
	case meta.IsNoMatchError(err):
		return naming.Owner{}, nil
	case err != nil:
		return naming.Owner{}, err
	}
	return naming.Owner{Found: true, Project: existing.GetLabels()[naming.LabelProject]}, nil
}

// recordProject writes the project onto a Cluster that carries none — the
// one thing a found Cluster is updated with, and only ever from empty.
func (c *CNPG) recordProject(ctx context.Context, cluster *unstructured.Unstructured, project string) error {
	if project == "" || cluster.GetLabels()[naming.LabelProject] != "" {
		return nil
	}
	labels := cluster.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[naming.LabelProject] = project
	cluster.SetLabels(labels)
	return c.Client.Update(ctx, cluster)
}

// initDB is the bootstrap block: a fresh database owned by a fresh role, with
// the claim's extensions created in it.
//
// The extensions are created here rather than left to the application because
// CREATE EXTENSION needs rights the application's own role does not have, and
// handing an application superuser so that its first migration works is how a
// platform ends up with no isolation at all. postInitApplicationSQL runs once,
// as superuser, in the application database — which is exactly the one moment
// that is true.
//
// Every name in it has been through extensionName: the statements are built
// by the platform out of a closed alphabet, never escaped out of whatever the
// claim happened to say.
func initDB(extensions []string) map[string]any {
	initdb := map[string]any{
		"database": applicationDatabase,
		"owner":    applicationUser,
	}
	if len(extensions) > 0 {
		statements := make([]any, 0, len(extensions))
		for _, extension := range extensions {
			statements = append(statements, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS "%s"`, extension))
		}
		initdb["postInitApplicationSQL"] = statements
	}
	return map[string]any{"initdb": initdb}
}

// ready reports whether the Cluster is serving, as ErrNotReady while it is
// not. A database the platform runs takes minutes to come up, and a claim
// waiting for one is Pending rather than Failed — the reconciler tells them
// apart by this error.
func (c *CNPG) ready(cluster *unstructured.Unstructured) error {
	conditions, found, err := unstructured.NestedSlice(cluster.Object, "status", "conditions")
	if err == nil && found {
		for _, entry := range conditions {
			condition, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if condition["type"] == "Ready" && condition["status"] == string(metav1.ConditionTrue) {
				return nil
			}
		}
	}
	phase := nestedString(cluster, "status", "phase")
	if phase == "" {
		phase = "no status yet"
	}
	return fmt.Errorf("%w: database %s is still coming up (%s)", ErrNotReady, cluster.GetName(), phase)
}

// binding reads the credentials CloudNativePG wrote for the application role.
//
// The host is built here rather than taken from the Secret on purpose: cnpg
// writes the Service's short name, which resolves in the database namespace
// and nowhere else — and every consumer of this binding is a pod in some
// project's application namespace. The fully qualified name is the one that
// works from both.
func (c *CNPG) binding(ctx context.Context, cluster string) (Binding, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: c.Namespace, Name: cluster + "-app"}
	if err := c.Client.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return Binding{}, fmt.Errorf("%w: database %s has not published its credentials yet", ErrNotReady, cluster)
		}
		return Binding{}, err
	}

	value := func(keys ...string) string {
		for _, name := range keys {
			if raw, ok := secret.Data[name]; ok && len(raw) > 0 {
				return string(raw)
			}
		}
		return ""
	}

	user := value("username", "user")
	password := value("password")
	database := value("dbname", "database")
	if user == "" || password == "" {
		return Binding{}, fmt.Errorf("%w: secret %s does not hold the application credentials yet", ErrNotReady, key.Name)
	}
	if database == "" {
		database = applicationDatabase
	}
	port := value("port")
	if port == "" {
		port = "5432"
	}
	host := fmt.Sprintf("%s-rw.%s.svc", cluster, c.Namespace)

	// `sslmode=require` rather than libpq's default, which is `prefer`:
	// prefer negotiates TLS and silently falls back to plaintext when the
	// server declines, so a downgrade is indistinguishable from a normal
	// connection. CloudNativePG serves TLS on every cluster it creates, with
	// a CA it generates itself, so requiring encryption costs nothing here.
	//
	// It is `require` and not `verify-full` because verification needs that
	// per-cluster CA in the application's trust store, and nothing puts it
	// there — the binding is one string handed to a container. Requiring
	// encryption without verification is what the same client already asks of
	// Neon (see neon.go), and it closes the plaintext-on-the-wire half.
	dsn := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(user, password),
		Host:     host + ":" + port,
		Path:     "/" + database,
		RawQuery: "sslmode=require",
	}
	return Binding{
		URL:      dsn.String(),
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		Database: database,
	}, nil
}

// region reports where the database actually runs, off the node its primary
// landed on. It is the provider contract's residency: reported, not declared,
// and empty when the cluster's nodes say nothing about their topology — which
// a single-node or an unlabelled cluster does, and which the claim records as
// unknown rather than filling in.
func (c *CNPG) region(ctx context.Context, cluster string) string {
	pods := &corev1.PodList{}
	if err := c.Client.List(ctx, pods,
		client.InNamespace(c.Namespace),
		client.MatchingLabels{clusterLabel: cluster},
	); err != nil || len(pods.Items) == 0 {
		return ""
	}

	node := ""
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		node = pod.Spec.NodeName
		if pod.Labels["role"] == "primary" || pod.Labels["cnpg.io/instanceRole"] == "primary" {
			break
		}
	}
	if node == "" {
		return ""
	}

	where := &corev1.Node{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: node}, where); err != nil {
		return ""
	}
	if region := where.Labels[regionLabel]; region != "" {
		return region
	}
	return where.Labels[zoneLabel]
}

// cluster reads a Cluster by the instance ID this provisioner minted.
func (c *CNPG) cluster(ctx context.Context, instanceID string) (*unstructured.Unstructured, error) {
	namespace, name := splitID(instanceID, c.Namespace)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cluster); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, notInstalled(err)
		}
		return nil, err
	}
	return cluster, nil
}

// deleteCluster removes one Cluster, treating an absent one — and a cluster
// that no longer serves cnpg at all — as already gone. A claim must be
// deletable after its operator has been uninstalled; wedging a finalizer on
// that is a worse state than a database nobody can find.
func (c *CNPG) deleteCluster(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	namespace, name := splitID(id, c.Namespace)
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK())
	cluster.SetNamespace(namespace)
	cluster.SetName(name)
	err := c.Client.Delete(ctx, cluster)
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

// notInstalled turns a RESTMapper no-match into the sentence somebody can
// act on.
func notInstalled(err error) error {
	return fmt.Errorf("%w: this cluster does not serve postgresql.cnpg.io/v1, so CloudNativePG is not installed. "+
		"Set spec.install on the cloudnative-pg addon to have the platform install it, or install it yourself "+
		"(%s)", ErrUnsatisfiable, err)
}

// splitID takes "namespace/name" apart, tolerating a bare name from a status
// an older operator wrote.
func splitID(id, fallback string) (string, string) {
	if namespace, name, found := strings.Cut(id, "/"); found {
		return namespace, name
	}
	return fallback, id
}

// nestedString is unstructured.NestedString with the two-value dance folded
// away; a field that is missing or is not a string reads as empty, which is
// what every caller here wants.
func nestedString(object *unstructured.Unstructured, fields ...string) string {
	value, found, err := unstructured.NestedString(object.Object, fields...)
	if err != nil || !found {
		return ""
	}
	return value
}

// maxClusterName keeps a Cluster's name inside what Kubernetes and
// cnpg will take. cnpg derives Service and Secret names from it by appending
// suffixes, so the budget is smaller than a label's 63: 50 leaves room for
// "-app", "-rw" and the instance ordinals.
const maxClusterName = 50

// branchName is the parent's name with the environment's appended, each
// trimmed so the pair fits — the environment half is what makes it unique, so
// it is the half that keeps the most room.
func branchName(parent, environment string) string {
	return naming.Join(parent, maxClusterName/2, environment, maxClusterName)
}

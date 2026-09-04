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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/provider/cache"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// A self-hosted Inngest: the single binary that is every one of Inngest's
// services (https://www.inngest.com/docs/self-hosting), run in the cluster
// Kitchen was installed into, one server per claim and one per preview.
//
// **A server per environment is the answer to the tenancy question.** #268
// shipped the claim against Inngest Cloud and named the open problem: two
// previews registering functions on the same event name into one server
// means an event from one can trigger a function in the other. App naming
// namespaces the apps; it does not namespace the event stream. Cloud solves
// it with branch environments, and a self-hosted server has no environments
// at all — so the only boundary left is the process. A preview therefore
// gets an Inngest of its own, which dissolves the question rather than
// documenting it, and the cost is bounded by what #294 shipped: the server
// parks with its preview (CanIdle), and the preview ceiling caps how many
// there can be. An installation that would rather have one server says so
// with previewMode: shared.
//
// **Storage is two shapes, and the difference is written down because it is
// visible.** Production's server gets what the Inngest docs ask for — an
// external Postgres for the system data and history, an external Redis for
// the queue and the run state — and gets them from the two in-cluster
// providers the platform already has: a CloudNativePG Cluster and a Valkey,
// the same code paths a postgres and a redis claim provision through. A
// preview's server gets Inngest's own embedded store instead: SQLite and the
// in-memory Redis, on a volume of its own. That is one pod per preview
// rather than three, on the single-node clusters this platform is built for,
// for an environment that is parked most of the time and holds nothing
// anybody has to keep — and it is the one respect in which a preview's
// Inngest is not production's shape.

const (
	// DefaultServerNamespace is where the servers run. It is deliberately
	// *not* a project's application namespace: that namespace is deleted
	// with its project, and the claim's own lifecycle is what should decide
	// when a server goes.
	DefaultServerNamespace = "kitchen-inngest"

	// DefaultServerImage is the Inngest the platform runs, pinned by tag the
	// way internal/controller/addon_keda.go pins its chart pair and
	// internal/provider/database pins its Postgres images: what the platform
	// runs is what the platform knows how to operate.
	//
	// Bumping it means reading Inngest's release notes for the flags this
	// file passes — the persistence flags above all, since they are what
	// decides whether production's history is in Postgres or in a SQLite
	// file nobody backs up. docs/CONFIG.md says so beside the value an
	// installation overrides it with.
	DefaultServerImage = "inngest/inngest:v1.44.0"

	// ServerPort is where the server serves the event API, the REST and
	// GraphQL API and its own dashboard, and ConnectGatewayPort is where a
	// connect worker's WebSocket lands. Both are Inngest's documented
	// defaults and neither is configurable: the binding carries the
	// addresses, and nothing on the other side of it needs a say.
	ServerPort         = 8288
	ConnectGatewayPort = 8289

	// DefaultPreviewStorage is the volume a preview's embedded store gets —
	// its SQLite database and the queue snapshots beside it.
	DefaultPreviewStorage = "1Gi"

	// pollInterval is how often a serve-mode server re-syncs the application
	// it was given the URL of, in seconds. Inngest polls nothing by default
	// (`--poll-interval 0`), which for a platform that redeploys the
	// application under the server would mean a function set frozen at the
	// first sync.
	pollInterval = 60

	// maxServerName is the name budget. Every object of a server is named
	// after it, the longest being the Postgres Cluster's name plus cnpg's
	// own suffixes, so the server's name is capped well short of 63;
	// internal/provider/naming keeps a name inside it without ever mapping
	// two names onto one.
	maxServerName = 40

	// The suffixes the two storage instances are named with, so that a
	// server, its database and its queue are three names in one namespace
	// rather than one name three objects fight over.
	postgresSuffix = "-db"
	cacheSuffix    = "-queue"

	// managedByLabel marks what this provisioner created, the way every
	// other object the platform writes is marked. An object without it was
	// somebody else's and is never written to or deleted.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kitchen"

	// serverLabel names the server an object belongs to, and is what the
	// Service selects its pod on.
	serverLabel = "kitchen.bermos.dev/inngest"

	// configAnnotation records the digest of everything the pod template is
	// built from — the image, the storage shape, the sync target — so that a
	// reconcile which changes one of them rolls the pods and one that
	// changes nothing writes nothing.
	configAnnotation = "kitchen.bermos.dev/inngest-config"

	// storageAnnotation records which of the two storage shapes a server was
	// made with, so that what a server keeps its state in can be read off
	// the server rather than inferred from what else is in the namespace.
	storageAnnotation = "kitchen.bermos.dev/inngest-storage"
)

// The keys the server itself reads out of its own Secret. They are the
// environment variables the Inngest binary takes — every CLI option is
// settable as INNGEST_<FLAG> — so the Deployment's envFrom is the whole of
// its configuration.
const (
	serverKeyPostgresURI = "INNGEST_POSTGRES_URI"
	serverKeyRedisURI    = "INNGEST_REDIS_URI"
)

// storageShape is where one server keeps its state.
type storageShape string

const (
	// storageExternal is Postgres and Redis of the server's own, provisioned
	// through the platform's in-cluster providers. Production's shape.
	storageExternal storageShape = "external"
	// storageEmbedded is Inngest's own SQLite and in-memory Redis on a
	// volume of the server's own. A preview's shape.
	storageEmbedded storageShape = "embedded"
)

// PostgresProvisioner is the slice of internal/provider/database this
// provider uses: a database of its own for one Inngest server. It is an
// interface rather than *database.CNPG so that a test can hand this
// provisioner a database without a CloudNativePG to make one in.
type PostgresProvisioner interface {
	ProvisionWith(ctx context.Context, res naming.Resource, req database.Requirements) (database.Instance, error)
	Deprovision(ctx context.Context, instanceID string) error
}

// CacheProvisioner is the same slice of internal/provider/cache: the queue
// and run state of one Inngest server.
type CacheProvisioner interface {
	ProvisionWith(ctx context.Context, res naming.Resource, req cache.Requirements) (cache.Instance, error)
	Deprovision(ctx context.Context, instanceID string) error
}

// SelfHosted runs one Inngest server per claim in the cluster Kitchen is
// installed in. It holds no credential: it writes through the operator's own
// service account, which is what makes inngestSelfHosted a Connection
// provider with nothing to store — and it mints the server's event key and
// signing key itself, because it is the server that checks them.
type SelfHosted struct {
	// Client is the platform's own cluster.
	Client client.Client
	// Namespace the servers run in.
	Namespace string
	// Image is the Inngest this installation runs.
	Image string
	// StorageSize and StorageClass are the volume behind a preview's
	// embedded store.
	StorageSize  string
	StorageClass string
	// Postgres and Cache are production's storage.
	Postgres PostgresProvisioner
	Cache    CacheProvisioner
}

// selfHostedConfig is the `inngestSelfHosted` Connection's spec.config:
// what every claim through this Connection inherits. All of it is the
// operator's — a developer asking for durable background work should not be
// able to choose the image it arrives in.
type selfHostedConfig struct {
	Namespace    string `json:"namespace,omitempty"`
	Image        string `json:"image,omitempty"`
	StorageSize  string `json:"storageSize,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
}

// NewSelfHosted builds the provisioner from a Connection, resolving the two
// storage providers to the platform's own when nothing injected them.
func NewSelfHosted(opts Options) (*SelfHosted, error) {
	if opts.Cluster == nil {
		return nil, fmt.Errorf("the %s provider runs Inngest in this cluster and was given no client to do it with",
			ProviderSelfHosted)
	}
	cfg := selfHostedConfig{}
	if conn := opts.Connection; conn != nil && conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid %s config: %w", ProviderSelfHosted, err)
		}
	}
	namespace := firstNonEmpty(cfg.Namespace, opts.Namespace, DefaultServerNamespace)
	provisioner := &SelfHosted{
		Client:       opts.Cluster,
		Namespace:    namespace,
		Image:        firstNonEmpty(cfg.Image, DefaultServerImage),
		StorageSize:  firstNonEmpty(cfg.StorageSize, DefaultPreviewStorage),
		StorageClass: cfg.StorageClass,
		Postgres:     opts.Postgres,
		Cache:        opts.Cache,
	}
	if provisioner.Postgres == nil {
		// The same provisioner a postgres claim goes through, pointed at
		// this provider's namespace: an Inngest server's database is not a
		// project's database and does not belong beside them.
		postgres, err := database.NewCNPG(database.Options{Cluster: opts.Cluster, Namespace: namespace})
		if err != nil {
			return nil, err
		}
		provisioner.Postgres = postgres
	}
	if provisioner.Cache == nil {
		valkey, err := cache.NewValkey(cache.Options{Cluster: opts.Cluster, Namespace: namespace})
		if err != nil {
			return nil, err
		}
		provisioner.Cache = valkey
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

// Provision runs the claim's own Inngest server and binds it.
//
// What it refuses, it refuses before creating anything: a mode this provider
// does not serve, and an Inngest environment by name — a self-hosted server
// has no environments, and asking for one means asking for something this
// provider cannot give rather than for a default it can.
func (s *SelfHosted) Provision(ctx context.Context, res naming.Resource, req Requirements) (Instance, error) {
	if err := s.check(req); err != nil {
		return Instance{}, err
	}
	name, err := naming.Resolve(ctx, res, naming.Provider{
		Kind: "Inngest server", Limit: maxServerName, Lookup: s.owner,
	})
	if err != nil {
		return Instance{}, err
	}
	binding, err := s.ensureServer(ctx, name, res.Project, storageExternal, req)
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ID:      s.id(name),
		Name:    name,
		Binding: binding,
		Reason:  "ServerReady",
		Message: fmt.Sprintf("Inngest %s is serving at %s, on a Postgres and a queue of its own",
			name, binding.BaseURL),
	}, nil
}

// check refuses what this provider cannot serve as asked.
func (s *SelfHosted) check(req Requirements) error {
	switch req.Mode {
	case "", ModeConnect, ModeServe:
	default:
		return fmt.Errorf("%w: mode %q is not a mode: %s are, and a self-hosted server serves both — connect "+
			"has the worker dial the server's gateway, serve has the server call the application",
			ErrUnsatisfiable, req.Mode, strings.Join(modeList(), " and "))
	}
	if req.Environment != "" && req.Environment != DefaultEnvironment {
		return fmt.Errorf("%w: this claim asks for the Inngest environment %q, and a self-hosted server has no "+
			"environments to select — that is what Inngest Cloud's branch environments are, and it is why a "+
			"preview here gets a server of its own instead. Drop config.inngest.environment, or claim through "+
			"an %s connection", ErrUnsatisfiable, req.Environment, ProviderCloud)
	}
	return nil
}

func modeList() []string {
	return []string{ModeConnect, ModeServe}
}

// CreateBranch gives a preview Environment an Inngest of its own: its own
// event stream, its own function set and its own run history, on the
// embedded store rather than on a Postgres and a queue of its own.
//
// There is no copy here and there is not going to be. A preview server that
// started from production's event history would run production's pending
// functions, which is the failure this whole contract exists to make
// impossible; it is empty, and declared synthetic where the claim records
// what a preview binds.
func (s *SelfHosted) CreateBranch(ctx context.Context, instanceID, name string, req Requirements) (Branch, error) {
	parent, err := s.name(instanceID)
	if err != nil {
		return Branch{}, err
	}
	project, err := s.project(ctx, parent)
	if err != nil {
		return Branch{}, err
	}
	// The environment's name goes in the hash rather than the head, because
	// two previews of one claim differ at the end of a long name far more
	// often than at the start.
	child := naming.Truncate(parent+"-"+name, maxServerName)
	binding, err := s.ensureServer(ctx, child, project, storageEmbedded, req)
	if err != nil {
		return Branch{}, err
	}
	return Branch{ID: s.id(child), Binding: binding}, nil
}

// DeleteBranch removes a preview's server and the volume under it.
func (s *SelfHosted) DeleteBranch(ctx context.Context, _, branchID string) error {
	return s.deleteServer(ctx, branchID)
}

// Deprovision destroys the claim's server, its Postgres and its queue.
//
// It is unconditional because the claim type carries no deletionPolicy:
// there is no third party holding anything here for a policy to choose
// about — every one of these objects is one this platform created for this
// claim. What that means for the run history is said where the claim is
// deleted, and in docs/api/claims.md.
func (s *SelfHosted) Deprovision(ctx context.Context, instanceID string) error {
	return s.deleteServer(ctx, instanceID)
}

// IdleBranch parks a preview's server at no pods while the preview it
// belongs to is parked (#294). The Service, the Secret and the volume its
// runs and its queue are on all stay: this is the Deployment's replica count
// and nothing else, so waking it is the same write in reverse and what was
// on disk is still there.
func (s *SelfHosted) IdleBranch(ctx context.Context, branchID string) error {
	return s.scale(ctx, branchID, 0)
}

// WakeBranch brings it back to its one replica. It returns once the
// Deployment has been asked, not once Inngest answers: the claim's own
// readiness path is what reports that.
func (s *SelfHosted) WakeBranch(ctx context.Context, branchID string) error {
	return s.scale(ctx, branchID, 1)
}

// scale writes a server's replica count, treating one that is already there
// as done and one that is gone as nothing to do. Parking must never be able
// to wedge a reconcile: the worst case is a preview that keeps running,
// which is what the platform did before this existed.
func (s *SelfHosted) scale(ctx context.Context, instanceID string, replicas int32) error {
	name, err := s.name(instanceID)
	if err != nil {
		return err
	}
	deployment := &appsv1.Deployment{}
	if err := s.Client.Get(ctx, s.key(name), deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == replicas {
		return nil
	}
	deployment.Spec.Replicas = ptr.To(replicas)
	if err := s.Client.Update(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// server is one server's resolved configuration: everything its objects are
// built from, with the claim's requirements and the Connection's defaults
// already reconciled.
type server struct {
	storage storageShape
	// serveURL is where the server calls the application in serve mode,
	// empty in connect mode and while the environment has no URL yet.
	serveURL string
	// project is whose server this is, written onto every object as the
	// label a later claim's adoption is judged against.
	project string
	// postgresURI and redisURI are the external store, empty when the
	// server keeps its own.
	postgresURI string
	redisURI    string
}

// ensureServer makes everything one server is, in the order that leaves
// nothing half-made: its storage first, because the Deployment reads the
// URIs; then the keys, because the Deployment mounts them; then the address;
// then the workload.
func (s *SelfHosted) ensureServer(
	ctx context.Context,
	name, project string,
	storage storageShape,
	req Requirements,
) (Binding, error) {
	if err := s.ensureNamespace(ctx); err != nil {
		return Binding{}, err
	}
	cfg := server{storage: storage, project: project}
	if req.Mode == ModeServe {
		cfg.serveURL = req.ServeURL
	}
	if storage == storageExternal {
		postgresURI, redisURI, err := s.ensureStorage(ctx, name, project)
		if err != nil {
			return Binding{}, err
		}
		cfg.postgresURI, cfg.redisURI = postgresURI, redisURI
	}
	keys, err := s.ensureSecret(ctx, name, cfg)
	if err != nil {
		return Binding{}, err
	}
	if err := s.ensureService(ctx, name, project); err != nil {
		return Binding{}, err
	}
	if storage == storageEmbedded {
		if err := s.ensureVolume(ctx, name, project); err != nil {
			return Binding{}, err
		}
	}
	ready, err := s.ensureDeployment(ctx, name, cfg)
	if err != nil {
		return Binding{}, err
	}
	if !ready {
		return Binding{}, fmt.Errorf("%w: Inngest %s is starting", ErrNotReady, name)
	}
	return s.binding(name, keys), nil
}

// ensureStorage provisions the Postgres and the queue production's server
// keeps its state in, through the platform's own in-cluster providers — the
// same code paths a postgres and a redis claim go through, so that an
// Inngest server's database is operated, backed up and upgraded exactly like
// an application's.
//
// The queue asks for `queue` rather than `cache` deliberately: what is in it
// is function runs nobody can recompute, and a cache's eviction policy would
// drop them under memory pressure and report nothing.
func (s *SelfHosted) ensureStorage(ctx context.Context, name, project string) (string, string, error) {
	postgres, err := s.Postgres.ProvisionWith(ctx,
		naming.Resource{Project: project, Name: name + postgresSuffix}, database.Requirements{})
	if err != nil {
		return "", "", storageError(err, "the Postgres behind Inngest "+name)
	}
	queue, err := s.Cache.ProvisionWith(ctx,
		naming.Resource{Project: project, Name: name + cacheSuffix}, cache.Requirements{Usage: cache.UsageQueue})
	if err != nil {
		return "", "", storageError(err, "the queue behind Inngest "+name)
	}
	return postgres.Binding.URL, queue.Binding.URL, nil
}

// storageError translates what the two storage providers answer into this
// contract's vocabulary, so that a claim waiting for a database to start
// reads Pending and one asking for something the cluster cannot give reads
// Failed with the provider's own words.
func storageError(err error, what string) error {
	switch {
	case errors.Is(err, database.ErrNotReady), errors.Is(err, cache.ErrNotReady):
		return fmt.Errorf("%w: %s is starting: %s", ErrNotReady, what, err.Error())
	case errors.Is(err, database.ErrUnsatisfiable), errors.Is(err, cache.ErrUnsatisfiable),
		errors.Is(err, naming.ErrNotAdoptable):
		return fmt.Errorf("%w: %s cannot be provisioned: %s", ErrUnsatisfiable, what, err.Error())
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// ensureNamespace creates the namespace the servers live in. It is bare —
// nothing but the managed-by label — because it belongs to the platform and
// not to any project, and because nothing here asks for a Pod Security
// relaxation.
func (s *SelfHosted) ensureNamespace(ctx context.Context) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   s.Namespace,
		Labels: map[string]string{managedByLabel: managedByValue},
	}}
	err := s.Client.Create(ctx, namespace)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// keyPair is a server's own event key and signing key.
type keyPair struct {
	event, signing string
}

// ensureSecret mints the server's key pair once and reads it back
// afterwards, and keeps the storage URIs beside it — the Deployment's whole
// configuration is this one object.
//
// The keys are minted here rather than derived from anything, so that
// nothing outside this Secret can reconstruct them, and they are hex because
// Inngest requires the signing key to be one
// (https://www.inngest.com/docs/self-hosting#configuration).
func (s *SelfHosted) ensureSecret(ctx context.Context, name string, cfg server) (keyPair, error) {
	existing := &corev1.Secret{}
	err := s.Client.Get(ctx, s.key(name), existing)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return keyPair{}, err
	default:
		keys := keyPair{
			event:   string(existing.Data[KeyEventKey]),
			signing: string(existing.Data[KeySigningKey]),
		}
		if keys.event == "" || keys.signing == "" {
			return keyPair{}, fmt.Errorf("the secret for Inngest %s holds no key pair", name)
		}
		// The storage URIs are re-written where they have moved — a database
		// whose password was rotated under the server — and the Deployment's
		// config digest is what rolls the pods onto them.
		if string(existing.Data[serverKeyPostgresURI]) != cfg.postgresURI ||
			string(existing.Data[serverKeyRedisURI]) != cfg.redisURI {
			if existing.Data == nil {
				existing.Data = map[string][]byte{}
			}
			existing.Data[serverKeyPostgresURI] = []byte(cfg.postgresURI)
			existing.Data[serverKeyRedisURI] = []byte(cfg.redisURI)
			if err := s.Client.Update(ctx, existing); err != nil {
				return keyPair{}, err
			}
		}
		return keys, nil
	}

	keys, err := newKeyPair()
	if err != nil {
		return keyPair{}, err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: s.labels(name, cfg.project)},
		Type:       corev1.SecretTypeOpaque,
		// Data rather than StringData: these are values the provisioner
		// reads back on every reconcile, and StringData is a convenience the
		// API server converts on write.
		Data: map[string][]byte{
			KeyEventKey:          []byte(keys.event),
			KeySigningKey:        []byte(keys.signing),
			serverKeyPostgresURI: []byte(cfg.postgresURI),
			serverKeyRedisURI:    []byte(cfg.redisURI),
		},
	}
	if err := s.Client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another reconcile got there first; read its keys rather than a
			// second pair nothing would be able to use.
			return s.ensureSecret(ctx, name, cfg)
		}
		return keyPair{}, err
	}
	return keys, nil
}

// newKeyPair is two lots of 32 random bytes, hex-encoded: an even number of
// hexadecimal characters, which is what the server requires of a signing key
// and what survives every URL, config file and shell either will pass
// through.
func newKeyPair() (keyPair, error) {
	keys := keyPair{}
	for _, into := range []*string{&keys.event, &keys.signing} {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return keyPair{}, err
		}
		*into = hex.EncodeToString(buf)
	}
	return keys, nil
}

// ensureService gives the server its address: the API and event port the
// binding's INNGEST_BASE_URL names, and the connect gateway beside it. A
// plain ClusterIP — an application reaches it by name, and nothing outside
// the cluster reaches it at all, which is the whole security story of a
// server that admits anyone holding its event key.
func (s *SelfHosted) ensureService(ctx context.Context, name, project string) error {
	err := s.Client.Get(ctx, s.key(name), &corev1.Service{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: s.labels(name, project)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{serverLabel: name},
			Ports: []corev1.ServicePort{
				{Name: "api", Port: ServerPort, TargetPort: intstr.FromInt32(ServerPort)},
				{Name: "connect", Port: ConnectGatewayPort, TargetPort: intstr.FromInt32(ConnectGatewayPort)},
			},
		},
	}
	return client.IgnoreAlreadyExists(s.Client.Create(ctx, service))
}

// ensureVolume is where a preview's embedded store lives: its SQLite
// database and the queue snapshots the server writes beside it. A
// PersistentVolumeClaim of its own rather than an emptyDir, because a
// preview that parks and wakes must find the work it left — parking is a
// replica count, and an emptyDir would go with the pod.
func (s *SelfHosted) ensureVolume(ctx context.Context, name, project string) error {
	err := s.Client.Get(ctx, s.key(name), &corev1.PersistentVolumeClaim{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	size, err := resource.ParseQuantity(s.StorageSize)
	if err != nil {
		return fmt.Errorf("%w: storageSize %q is not a Kubernetes quantity: %s",
			ErrUnsatisfiable, s.StorageSize, err.Error())
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace, Labels: s.labels(name, project)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if s.StorageClass != "" {
		claim.Spec.StorageClassName = ptr.To(s.StorageClass)
	}
	return client.IgnoreAlreadyExists(s.Client.Create(ctx, claim))
}

// ensureDeployment makes the workload and reports whether it is serving.
//
// A Deployment rather than a StatefulSet: there is one replica and its
// identity is its Service, and the volume a preview's embedded store needs
// is a claim of its own rather than a template. Recreate rather than
// RollingUpdate for the same volume: two pods rolling over each other would
// both want a ReadWriteOnce disk, and the second would never schedule.
//
// The replica count of a server that already exists is never written here.
// It belongs to the parking half of #294 — a preview whose environment is
// idle is at zero, and rewriting it on every reconcile would wake it.
func (s *SelfHosted) ensureDeployment(ctx context.Context, name string, cfg server) (bool, error) {
	desired := s.desiredDeployment(name, cfg)
	existing := &appsv1.Deployment{}
	err := s.Client.Get(ctx, s.key(name), existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := s.Client.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, err
		}
		// Just created: nothing is serving yet, and the caller waits.
		return false, nil
	case err != nil:
		return false, err
	}
	if err := s.recordProject(ctx, existing, cfg.project); err != nil {
		return false, err
	}
	if existing.Annotations[configAnnotation] != desired.Annotations[configAnnotation] {
		replicas := existing.Spec.Replicas
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[configAnnotation] = desired.Annotations[configAnnotation]
		existing.Annotations[storageAnnotation] = desired.Annotations[storageAnnotation]
		existing.Spec = desired.Spec
		existing.Spec.Replicas = replicas
		if err := s.Client.Update(ctx, existing); err != nil {
			return false, err
		}
		return false, nil
	}
	return existing.Status.ReadyReplicas >= 1, nil
}

// desiredDeployment is the whole of what a server runs.
func (s *SelfHosted) desiredDeployment(name string, cfg server) *appsv1.Deployment {
	labels := s.labels(name, cfg.project)
	env := serverEnv(cfg)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				configAnnotation:  digest(s.Image, string(cfg.storage), cfg.serveURL, envDigest(env)),
				storageAnnotation: string(cfg.storage),
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{serverLabel: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To(int64(1000)),
						FSGroup:        ptr.To(int64(1000)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:    "inngest",
						Image:   s.Image,
						Command: []string{"inngest"},
						Args:    []string{"start"},
						Ports: []corev1.ContainerPort{
							{Name: "api", ContainerPort: ServerPort},
							{Name: "connect", ContainerPort: ConnectGatewayPort},
						},
						Env: env,
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: name},
							},
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						ReadinessProbe: healthProbe(),
						LivenessProbe:  healthProbe(),
						VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: dataDir}},
					}},
					Volumes: []corev1.Volume{dataVolume(name, cfg.storage)},
				},
			},
		},
	}
	return deployment
}

// dataDir is the server's working directory: where the embedded store's
// SQLite database goes, and where a server on external storage writes
// nothing — it has one because the container's root filesystem is read-only
// and Inngest wants a directory whether or not it puts anything in it.
const dataDir = "/data"

// serverEnv is everything the server is told about itself that is not its
// key pair or its storage URIs — those come out of its Secret, whole.
func serverEnv(cfg server) []corev1.EnvVar {
	env := []corev1.EnvVar{
		// Every CLI option is settable as INNGEST_<FLAG>
		// (https://www.inngest.com/docs/self-hosting#configuration), which
		// is why there is no shell and no argument list to get wrong here.
		{Name: "INNGEST_PORT", Value: strconv.Itoa(ServerPort)},
		{Name: "INNGEST_CONNECT_GATEWAY_PORT", Value: strconv.Itoa(ConnectGatewayPort)},
		{Name: "INNGEST_SQLITE_DIR", Value: dataDir},
	}
	if cfg.serveURL != "" {
		// Serve mode: the server calls the application, so it has to be told
		// where the application is and to look again — the platform
		// redeploys underneath it, and a server polling nothing would hold
		// the function set of the first sync forever.
		env = append(env,
			corev1.EnvVar{Name: "INNGEST_SDK_URL", Value: cfg.serveURL},
			corev1.EnvVar{Name: "INNGEST_POLL_INTERVAL", Value: strconv.Itoa(pollInterval)},
		)
	}
	return env
}

// dataVolume is the preview's PersistentVolumeClaim, and an emptyDir for a
// server whose state is in a Postgres and a queue of its own.
func dataVolume(name string, storage storageShape) corev1.Volume {
	if storage == storageEmbedded {
		return corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name},
			},
		}
	}
	return corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// healthProbe asks the server whether it is answering, which for Inngest is
// one endpoint (https://www.inngest.com/docs/self-hosting#docker-compose-example).
func healthProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(ServerPort)},
		},
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
	}
}

// binding is the address and the keys of a server this provisioner made. It
// is built rather than read back: the Service name is the host by
// construction, and the keys came from the Secret above.
func (s *SelfHosted) binding(name string, keys keyPair) Binding {
	host := fmt.Sprintf("%s.%s.svc", name, s.Namespace)
	return Binding{
		EventKey:   keys.event,
		SigningKey: keys.signing,
		// A self-hosted server has no environments: a preview gets a server
		// of its own, which is what INNGEST_ENV would otherwise have been
		// for.
		Env:     "",
		BaseURL: fmt.Sprintf("http://%s:%d", host, ServerPort),
		// Cloud mode with the base URL pointed here, which is what the
		// self-hosting guide says to set: signatures are verified, and the
		// SDK does not go looking for Inngest Cloud.
		Dev:               "0",
		ConnectGatewayURL: fmt.Sprintf("ws://%s:%d/v0/connect", host, ConnectGatewayPort),
	}
}

// deleteServer removes everything one server is, including the storage
// behind it. The two storage providers are asked under either shape: a
// preview's server has neither, and both are tolerant of what is not there,
// which is cheaper than recording which shape it was and trusting the
// record.
func (s *SelfHosted) deleteServer(ctx context.Context, instanceID string) error {
	name, err := s.name(instanceID)
	if err != nil {
		return err
	}
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}},
	} {
		if err := s.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := s.Postgres.Deprovision(ctx, s.id(name+postgresSuffix)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := s.Cache.Deprovision(ctx, s.id(name+cacheSuffix)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (s *SelfHosted) labels(name, project string) map[string]string {
	labels := map[string]string{
		serverLabel:              name,
		managedByLabel:           managedByValue,
		"app.kubernetes.io/name": "inngest",
	}
	if project != "" {
		labels[naming.LabelProject] = project
	}
	return labels
}

// owner answers naming.Lookup: whether a server of that name is there and
// which project it was created for. The Deployment is the server — the
// Secret, the Service and the volume are made beside it and go with it — so
// it is the one object asked.
func (s *SelfHosted) owner(ctx context.Context, name string) (naming.Owner, error) {
	existing := &appsv1.Deployment{}
	err := s.Client.Get(ctx, s.key(name), existing)
	switch {
	case apierrors.IsNotFound(err):
		return naming.Owner{}, nil
	case err != nil:
		return naming.Owner{}, err
	}
	return naming.Owner{Found: true, Project: existing.Labels[naming.LabelProject]}, nil
}

// project reads whose server one is, so that a preview's is labelled like
// the one it branches from without the claim being read again.
func (s *SelfHosted) project(ctx context.Context, name string) (string, error) {
	existing := &appsv1.Deployment{}
	if err := s.Client.Get(ctx, s.key(name), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return existing.Labels[naming.LabelProject], nil
}

// recordProject writes the project onto a server that carries none — a
// server an operator has just handed over records whose it is from now on.
func (s *SelfHosted) recordProject(ctx context.Context, deployment *appsv1.Deployment, project string) error {
	if project == "" || deployment.Labels[naming.LabelProject] != "" {
		return nil
	}
	if deployment.Labels == nil {
		deployment.Labels = map[string]string{}
	}
	deployment.Labels[naming.LabelProject] = project
	return s.Client.Update(ctx, deployment)
}

// id is the opaque instance identifier the claim records: namespace/name,
// which is what every operation above addresses a server by.
func (s *SelfHosted) id(name string) string { return s.Namespace + "/" + name }

// name takes an instance ID apart. An ID that is not one is an error rather
// than a guess: it came off a claim's status, and acting on half of it would
// address the wrong object.
func (s *SelfHosted) name(instanceID string) (string, error) {
	parts := strings.SplitN(instanceID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("%q is not a namespace/name instance id", instanceID)
	}
	return parts[1], nil
}

func (s *SelfHosted) key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: s.Namespace, Name: name}
}

// digest is the config annotation's value: everything a pod template is
// built from, hashed, so that comparing two of them is comparing one string.
func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

// envDigest is the environment as one string, in a fixed order — a digest
// that moved because a map iterated differently would roll the pods on every
// reconcile.
func envDigest(env []corev1.EnvVar) string {
	pairs := make([]string, 0, len(env))
	for _, v := range env {
		pairs = append(pairs, v.Name+"="+v.Value)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}

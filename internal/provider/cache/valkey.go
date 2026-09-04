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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

const (
	// DefaultCacheNamespace is where the instances go. It is deliberately
	// *not* the project's application namespace: deleting a project deletes
	// that namespace, and a claim under deletionPolicy Retain must survive
	// exactly that — the same reasoning, and the same shape, as the
	// database contract's kitchen-databases.
	DefaultCacheNamespace = "kitchen-caches"

	// DefaultMaxMemory is what an instance whose claim names no limit gets.
	// Small enough to cost nothing on any cluster, and large enough that a
	// cache is worth having; a claim says so when it wants more.
	DefaultMaxMemory = "256Mi"

	// DefaultQueueStorage is the volume behind a queue. A queue's whole
	// point is that what is in it survives a restart, so it gets a volume
	// where a cache gets none.
	DefaultQueueStorage = "1Gi"

	// valkeyPort is the port every Valkey serves on. It is not configurable:
	// the binding carries the address, and nothing on the other side of it
	// needs a say.
	valkeyPort = 6379

	// managedByLabel marks the instances this provisioner created, the way
	// every other object the platform writes is marked. An object without it
	// was somebody else's and is never written to or deleted.
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "kitchen"

	// instanceLabel names the instance an object belongs to, and is what the
	// Service selects its pod on.
	instanceLabel = "kitchen.bermos.dev/cache"

	// usageAnnotation and versionAnnotation record what the instance was
	// created as, so that a preview's instance can be configured like the
	// one it branches from without the claim being read again.
	usageAnnotation     = "kitchen.bermos.dev/cache-usage"
	maxMemoryAnnotation = "kitchen.bermos.dev/cache-max-memory"
	imageAnnotation     = "kitchen.bermos.dev/cache-image"
)

// DefaultValkeyImages is the catalogue of images this provisioner will run,
// newest first, pinned by digest-bearing tag. It is compiled in for the
// reason every other catalogue in this repository is: what the platform runs
// is what the platform knows how to operate, and a claim naming a version
// nothing here publishes is refused rather than run.
//
// An installation that wants another one says so on the Connection, which is
// the operator's to set — a developer asking for a version should not be
// able to choose the image it arrives in.
var DefaultValkeyImages = []ValkeyImage{
	{Major: "8", Image: "valkey/valkey:8.1-alpine"},
	{Major: "7", Image: "valkey/valkey:7.2-alpine"},
}

// ValkeyImage is one entry of that catalogue.
type ValkeyImage struct {
	// Major is the Valkey major version, as a claim names it: "8".
	Major string `json:"major"`
	// Image is what the pod runs.
	Image string `json:"image"`
}

// DefaultValkeyMajor is what a claim that names no version gets.
const DefaultValkeyMajor = "8"

// Valkey provisions one instance per claim into the cluster Kitchen is
// installed in. It holds no credential: it writes through the operator's
// service account, which is what makes `valkey` a Connection provider with
// nothing to store.
//
// One instance per claim, and never a database number inside a shared
// server. The package comment says why; the short version is that
// maxmemory-policy is server-wide, so the one requirement this contract
// exists for cannot be given to two tenants at once.
type Valkey struct {
	// Client is the platform's own cluster.
	Client client.Client
	// Namespace the instances go in.
	Namespace string
	// Images is the catalogue this provisioner will run.
	Images []ValkeyImage
	// MaxMemory is what a claim naming no limit gets.
	MaxMemory string
	// StorageSize and StorageClass are the volume behind a queue.
	StorageSize  string
	StorageClass string
}

// valkeyConfig is the `valkey` Connection's spec.config: what every claim
// through this Connection inherits, and the catalogue of images the
// installation will actually run. Both are the operator's to set.
type valkeyConfig struct {
	Namespace    string        `json:"namespace,omitempty"`
	MaxMemory    string        `json:"maxMemory,omitempty"`
	StorageSize  string        `json:"storageSize,omitempty"`
	StorageClass string        `json:"storageClass,omitempty"`
	Images       []ValkeyImage `json:"images,omitempty"`
}

// NewValkey builds the provisioner from a Connection.
func NewValkey(opts Options) (*Valkey, error) {
	if opts.Cluster == nil {
		return nil, fmt.Errorf("the %s provider provisions into this cluster and was given no client to do it with",
			ProviderValkey)
	}
	cfg := valkeyConfig{}
	if conn := opts.Connection; conn != nil && conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid %s config: %w", ProviderValkey, err)
		}
	}
	provisioner := &Valkey{
		Client:       opts.Cluster,
		Namespace:    firstNonEmpty(cfg.Namespace, opts.Namespace, DefaultCacheNamespace),
		Images:       cfg.Images,
		MaxMemory:    firstNonEmpty(cfg.MaxMemory, DefaultMaxMemory),
		StorageSize:  firstNonEmpty(cfg.StorageSize, DefaultQueueStorage),
		StorageClass: cfg.StorageClass,
	}
	if len(provisioner.Images) == 0 {
		provisioner.Images = DefaultValkeyImages
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

// Provision creates an instance with nothing asked of it beyond a name.
func (v *Valkey) Provision(ctx context.Context, res naming.Resource) (Instance, error) {
	return v.ProvisionWith(ctx, res, Requirements{})
}

// ProvisionWith creates (or finds) the instance for a claim.
//
// The requirements are resolved *first*, before anything is created: a claim
// asking for a version nothing publishes, or a memory limit that is not a
// quantity, is refused as a claim rather than left as a pod that will not
// start.
func (v *Valkey) ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error) {
	settings, err := v.resolve(req)
	if err != nil {
		return Instance{}, err
	}
	name, err := naming.Resolve(ctx, res, naming.Provider{
		Kind: "cache instance", Limit: maxInstanceName, Lookup: v.owner,
	})
	if err != nil {
		return Instance{}, err
	}
	settings.project = res.Project
	binding, err := v.ensureInstance(ctx, name, settings)
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ID:      v.Namespace + "/" + name,
		Name:    name,
		Binding: binding,
		// An instance the platform provisions for a project holds that
		// project's own data, and the claim's is production's. Only a
		// preview's is synthetic, and CreateBranch is where that is said.
		Provenance: ProvenanceProduction,
	}, nil
}

// CreateBranch gives a preview Environment its own instance, configured like
// the one it branches from and holding none of its keys.
//
// There is no copy here and there is not going to be: a cache's contents are
// by definition recomputable, and a queue's are somebody's jobs — copying
// production's queue into a preview would have the preview's workers execute
// them. A preview gets an empty instance, declared synthetic, which is both
// true and the state an auditor wants the platform unable to leave.
func (v *Valkey) CreateBranch(ctx context.Context, instanceID, name string) (Branch, error) {
	settings, err := v.inherit(ctx, instanceID)
	if err != nil {
		return Branch{}, err
	}
	child := branchName(instanceID, name)
	binding, err := v.ensureInstance(ctx, child, settings)
	if err != nil {
		return Branch{}, err
	}
	return Branch{
		ID:         v.Namespace + "/" + child,
		Binding:    binding,
		Provenance: ProvenanceSynthetic,
	}, nil
}

// Deprovision destroys the instance and everything in it.
func (v *Valkey) Deprovision(ctx context.Context, instanceID string) error {
	return v.deleteInstance(ctx, instanceID)
}

// DeleteBranch removes a preview's instance.
func (v *Valkey) DeleteBranch(ctx context.Context, _, branchID string) error {
	return v.deleteInstance(ctx, branchID)
}

// settings is one instance's resolved configuration: everything the objects
// are built from, with the claim's requirements and the Connection's
// defaults already reconciled.
type settings struct {
	usage     Usage
	image     string
	maxMemory resource.Quantity
	// project is whose instance this is, written onto every object as the
	// label a later claim's adoption is judged against.
	project string
}

// resolve turns a claim's requirements into settings, refusing before
// anything is created what this provisioner cannot supply.
func (v *Valkey) resolve(req Requirements) (settings, error) {
	usage := req.Usage
	if usage == "" {
		usage = UsageCache
	}
	if !usage.Known() {
		return settings{}, fmt.Errorf("%w: usage %q is not one of %s", ErrUnsatisfiable, req.Usage, usageList())
	}

	major := firstNonEmpty(req.Version, DefaultValkeyMajor)
	image := ""
	for _, entry := range v.Images {
		if entry.Major == major {
			image = entry.Image
		}
	}
	if image == "" {
		return settings{}, fmt.Errorf("%w: no image for Valkey %s; this connection can run %s",
			ErrUnsatisfiable, major, v.majors())
	}

	quantity, err := resource.ParseQuantity(firstNonEmpty(req.MaxMemory, v.MaxMemory))
	if err != nil {
		return settings{}, fmt.Errorf("%w: maxMemory %q is not a Kubernetes quantity: %s",
			ErrUnsatisfiable, req.MaxMemory, err.Error())
	}
	if quantity.Sign() <= 0 {
		return settings{}, fmt.Errorf("%w: maxMemory must be more than nothing (got %q)",
			ErrUnsatisfiable, req.MaxMemory)
	}
	return settings{usage: usage, image: image, maxMemory: quantity}, nil
}

// majors is what this provisioner can run, for the refusal above.
func (v *Valkey) majors() string {
	majors := make([]string, 0, len(v.Images))
	for _, entry := range v.Images {
		majors = append(majors, entry.Major)
	}
	sort.Strings(majors)
	return strings.Join(majors, ", ")
}

func usageList() string {
	names := make([]string, 0, len(Usages))
	for _, usage := range Usages {
		names = append(names, string(usage))
	}
	return strings.Join(names, " or ")
}

// inherit reads the settings an existing instance was created with, so a
// preview's is configured like the one it branches from. A claim's queue
// gets a preview queue, not a preview cache that would drop its jobs.
func (v *Valkey) inherit(ctx context.Context, instanceID string) (settings, error) {
	_, name, err := splitID(instanceID)
	if err != nil {
		return settings{}, err
	}
	parent := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: v.Namespace, Name: name}
	if err := v.Client.Get(ctx, key, parent); err != nil {
		return settings{}, err
	}
	usage := Usage(parent.Annotations[usageAnnotation])
	if !usage.Known() {
		usage = UsageCache
	}
	quantity, err := resource.ParseQuantity(firstNonEmpty(parent.Annotations[maxMemoryAnnotation], v.MaxMemory))
	if err != nil {
		return settings{}, err
	}
	image := parent.Annotations[imageAnnotation]
	if image == "" && len(parent.Spec.Template.Spec.Containers) > 0 {
		image = parent.Spec.Template.Spec.Containers[0].Image
	}
	return settings{
		usage: usage, image: image, maxMemory: quantity,
		project: parent.Labels[naming.LabelProject],
	}, nil
}

// ensureInstance makes the three objects an instance is, in the order that
// leaves nothing half-made: the password first, because the StatefulSet
// mounts it, then the Service, then the workload.
func (v *Valkey) ensureInstance(ctx context.Context, name string, cfg settings) (Binding, error) {
	if err := v.ensureNamespace(ctx); err != nil {
		return Binding{}, err
	}
	password, err := v.ensureSecret(ctx, name, cfg.project)
	if err != nil {
		return Binding{}, err
	}
	if err := v.ensureService(ctx, name, cfg.project); err != nil {
		return Binding{}, err
	}
	ready, err := v.ensureStatefulSet(ctx, name, cfg)
	if err != nil {
		return Binding{}, err
	}
	if !ready {
		return Binding{}, fmt.Errorf("%w: %s is starting", ErrNotReady, name)
	}
	return v.binding(name, password), nil
}

// ensureNamespace creates the namespace the instances live in. It is bare —
// nothing but the managed-by label — because it belongs to the platform and
// not to any project.
func (v *Valkey) ensureNamespace(ctx context.Context) error {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   v.Namespace,
		Labels: map[string]string{managedByLabel: managedByValue},
	}}
	err := v.Client.Create(ctx, namespace)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// ensureSecret mints the instance's password once and reads it back
// afterwards. It is minted here rather than derived from anything, so that
// nothing outside this Secret can reconstruct it.
func (v *Valkey) ensureSecret(ctx context.Context, name, project string) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: v.Namespace, Name: name}
	err := v.Client.Get(ctx, key, secret)
	switch {
	case err == nil:
		password := string(secret.Data[BindingKeyPassword])
		if password == "" {
			return "", fmt.Errorf("the secret for %s holds no %s", name, BindingKeyPassword)
		}
		return password, nil
	case !apierrors.IsNotFound(err):
		return "", err
	}

	password, err := newPassword()
	if err != nil {
		return "", err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: v.Namespace,
			Labels:    v.labels(name, project),
		},
		Type: corev1.SecretTypeOpaque,
		// Data rather than StringData: this is a value the provisioner reads
		// back on every reconcile, and StringData is a convenience the API
		// server converts on write — writing Data means the password
		// round-trips identically wherever it is stored.
		Data: map[string][]byte{BindingKeyPassword: []byte(password)},
	}
	if err := v.Client.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another reconcile got there first; read its password rather
			// than a second one nothing would be able to use.
			return v.ensureSecret(ctx, name, project)
		}
		return "", err
	}
	return password, nil
}

// newPassword is 32 bytes of randomness, hex-encoded so that it survives
// every URL, config file and shell it will pass through.
func newPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ensureService gives the instance its address. A plain ClusterIP: an
// application reaches it by name, and nothing outside the cluster reaches it
// at all.
func (v *Valkey) ensureService(ctx context.Context, name, project string) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: v.Namespace}}
	err := v.Client.Get(ctx, types.NamespacedName{Namespace: v.Namespace, Name: name}, service)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: v.Namespace, Labels: v.labels(name, project)},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{instanceLabel: name},
			Ports: []corev1.ServicePort{{
				Name: "valkey", Port: valkeyPort, TargetPort: intstrFromInt(valkeyPort),
			}},
		},
	}
	return client.IgnoreAlreadyExists(v.Client.Create(ctx, service))
}

// ensureStatefulSet makes the workload and reports whether it is serving.
//
// A StatefulSet rather than a Deployment for the reason the bundled registry
// is one: a queue's instance is a directory on a ReadWriteOnce volume, and
// two replicas rolling over each other would both want it. A cache has no
// volume and is a StatefulSet anyway, so that the two differ in their
// arguments and in nothing else.
func (v *Valkey) ensureStatefulSet(ctx context.Context, name string, cfg settings) (bool, error) {
	existing := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: v.Namespace, Name: name}
	err := v.Client.Get(ctx, key, existing)
	switch {
	case err == nil:
		// The project label is the one thing written onto an instance that
		// is already there, and only where there is none: an instance an
		// operator has just handed over records whose it is from now on.
		if err := v.recordProject(ctx, existing, cfg.project); err != nil {
			return false, err
		}
		return existing.Status.ReadyReplicas >= 1, nil
	case !apierrors.IsNotFound(err):
		return false, err
	}
	if err := v.Client.Create(ctx, v.desiredStatefulSet(name, cfg)); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, err
	}
	// Just created: nothing is serving yet, and the caller waits.
	return false, nil
}

// desiredStatefulSet is the whole of what an instance runs.
func (v *Valkey) desiredStatefulSet(name string, cfg settings) *appsv1.StatefulSet {
	labels := v.labels(name, cfg.project)
	replicas := int32(1)
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: v.Namespace,
			Labels:    labels,
			Annotations: map[string]string{
				usageAnnotation:     string(cfg.usage),
				maxMemoryAnnotation: cfg.maxMemory.String(),
				imageAnnotation:     cfg.image,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{instanceLabel: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    int64Ptr(1000),
						FSGroup:      int64Ptr(1000),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:  "valkey",
						Image: cfg.image,
						Args:  valkeyArgs(cfg),
						Ports: []corev1.ContainerPort{{Name: "valkey", ContainerPort: valkeyPort}},
						Env: []corev1.EnvVar{{
							Name: "VALKEY_PASSWORD",
							ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: name},
								Key:                  BindingKeyPassword,
							}},
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							ReadOnlyRootFilesystem:   boolPtr(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								// Headroom over maxmemory: Valkey's own
								// bookkeeping lives outside the keyspace, and
								// a limit set exactly at maxmemory is an OOM
								// kill rather than an eviction.
								corev1.ResourceMemory: memoryLimit(cfg.maxMemory),
							},
						},
						ReadinessProbe: valkeyProbe(),
						LivenessProbe:  valkeyProbe(),
						VolumeMounts:   volumeMounts(),
					}},
					Volumes: podVolumes(cfg),
				},
			},
		},
	}
	if cfg.usage.Durable() {
		set.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{Name: "data"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(v.StorageSize)},
				},
				StorageClassName: storageClass(v.StorageClass),
			},
		}}
	}
	return set
}

// valkeyArgs is where the whole contract lands: the difference between a
// cache and a queue is four arguments, and getting them backwards is the
// incident this contract exists to prevent.
func valkeyArgs(cfg settings) []string {
	args := []string{
		"--requirepass", "$(VALKEY_PASSWORD)",
		"--maxmemory", strconv.FormatInt(cfg.maxMemory.Value(), 10),
	}
	if cfg.usage.Durable() {
		// A queue holds work nobody can recompute. It refuses writes when it
		// is full — loudly, where the application can retry — and appends
		// every write to disk so a restart does not empty it.
		return append(args,
			"--maxmemory-policy", "noeviction",
			"--appendonly", "yes",
			"--dir", "/data",
		)
	}
	// A cache holds what can be recomputed. It evicts the least recently
	// used key when it fills up and writes nothing to disk: losing a key is
	// a miss, and the alternative is an instance that stops accepting writes
	// because it is full of things nobody needed to keep.
	return append(args,
		"--maxmemory-policy", "allkeys-lru",
		"--appendonly", "no",
		"--save", "",
	)
}

// valkeyProbe asks the server whether it is answering, which for Valkey is
// one command.
func valkeyProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstrFromInt(valkeyPort)},
		},
		InitialDelaySeconds: 2,
		PeriodSeconds:       10,
	}
}

// dataMount is where both usages keep their working directory. A queue's is
// the volume below; a cache's is an emptyDir it never writes to, and it
// exists because the container's root filesystem is read-only and Valkey
// wants a directory whether or not it puts anything there.
func volumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}
}

// podVolumes is the cache's emptyDir, and nothing for a queue — whose
// volume comes from the StatefulSet's claim template instead.
func podVolumes(cfg settings) []corev1.Volume {
	if cfg.usage.Durable() {
		return nil
	}
	return []corev1.Volume{{
		Name:         "data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
}

// memoryLimit is maxmemory plus a quarter, which is the headroom Valkey's
// own overhead needs. Without it the container is killed at exactly the
// point the eviction policy was meant to take over.
func memoryLimit(maxMemory resource.Quantity) resource.Quantity {
	bytes := maxMemory.Value()
	return *resource.NewQuantity(bytes+bytes/4, resource.BinarySI)
}

// binding is the address and credential of an instance this provisioner
// made. It is built rather than read back: the Service name is the host by
// construction, and the password came from the Secret above.
func (v *Valkey) binding(name, password string) Binding {
	host := fmt.Sprintf("%s.%s.svc", name, v.Namespace)
	port := strconv.Itoa(valkeyPort)
	return Binding{
		URL:      (&url.URL{Scheme: "redis", User: url.UserPassword("", password), Host: host + ":" + port}).String(),
		Host:     host,
		Port:     port,
		Password: password,
		// An instance of the claim's own is a server of the claim's own, so
		// there is nothing to allocate: everything lands in database 0, and
		// nothing else is in there.
		Database: "0",
		// In-cluster and unencrypted: the platform's own network is the
		// boundary, the way it is for every other in-cluster address the
		// platform hands out.
		TLS: false,
	}
}

// deleteInstance removes the three objects and, for a queue, the volume the
// StatefulSet leaves behind — a StatefulSet's volumeClaimTemplates PVCs are
// not garbage-collected with it, so deprovisioning without this would leave
// the data and its cost behind under a policy that asked for neither.
func (v *Valkey) deleteInstance(ctx context.Context, instanceID string) error {
	namespace, name, err := splitID(instanceID)
	if err != nil {
		return err
	}
	if namespace == "" {
		namespace = v.Namespace
	}
	for _, object := range []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
	} {
		if err := v.Client.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := v.Client.List(ctx, pvcs, client.InNamespace(namespace),
		client.MatchingLabels{instanceLabel: name}); err != nil {
		return err
	}
	for i := range pvcs.Items {
		if err := v.Client.Delete(ctx, &pvcs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (v *Valkey) labels(name, project string) map[string]string {
	labels := map[string]string{
		instanceLabel:            name,
		managedByLabel:           managedByValue,
		"app.kubernetes.io/name": "valkey",
	}
	if project != "" {
		labels[naming.LabelProject] = project
	}
	return labels
}

// owner answers naming.Lookup: whether an instance of that name is there and
// which project it was created for. The StatefulSet is the instance — the
// Secret and the Service are made beside it and go with it — so it is the
// one object asked.
func (v *Valkey) owner(ctx context.Context, name string) (naming.Owner, error) {
	existing := &appsv1.StatefulSet{}
	err := v.Client.Get(ctx, types.NamespacedName{Namespace: v.Namespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return naming.Owner{}, nil
	case err != nil:
		return naming.Owner{}, err
	}
	return naming.Owner{Found: true, Project: existing.Labels[naming.LabelProject]}, nil
}

// recordProject writes the project onto an instance that carries none.
func (v *Valkey) recordProject(ctx context.Context, set *appsv1.StatefulSet, project string) error {
	if project == "" || set.Labels[naming.LabelProject] != "" {
		return nil
	}
	if set.Labels == nil {
		set.Labels = map[string]string{}
	}
	set.Labels[naming.LabelProject] = project
	return v.Client.Update(ctx, set)
}

func storageClass(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }

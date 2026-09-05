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
	"path"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// The one object a bound volume claim needs that nothing on the platform
// made.
//
// #397 let a claim mount storage the platform did not create, and for the
// case that turns up most — an NFS export on a NAS older than the cluster —
// the thing to mount is a PersistentVolume. Nothing here wrote one, so the
// whole of that feature rested on a line of docs saying `kubectl apply -f
// media-pv.yaml`, which is the sentence CLAUDE.md says must not be true.
// This is that step, as a route and a screen.
//
// **It writes the PersistentVolume itself; there is no CRD behind it.** The
// house rule is that a write surface waits for its reconciler, and the
// reason is that an API over objects nothing reconciles only looks like it
// works. Here there is nothing left to reconcile: a PersistentVolume *is*
// the desired state, the API server admits it, and the platform has no
// second opinion to keep applying to it. A Kitchen CRD in front of it would
// be a second copy of a Kubernetes object, kept in step by a controller that
// exists only to copy — the two-sources-of-truth failure, bought for
// nothing. So this writes the object directly, exactly the way the
// connection routes write a Secret, and the label below is what makes the
// write accountable afterwards.
//
// Three decisions are worth reading before the code:
//
//   - **The platform owns what it wrote, and only that.** Every volume this
//     route creates carries `app.kubernetes.io/managed-by: kitchen`; the
//     listing answers those alone and the delete refuses anything else. A
//     PersistentVolume somebody wrote by hand is not the platform's to list
//     or to remove, which is the same line the connection secrets draw.
//   - **The reclaim policy is `Retain`, always, and cannot be asked for
//     otherwise.** These volumes point at data that existed before the
//     cluster; `Delete` would hand a storage driver permission to destroy
//     twelve terabytes of somebody's media when a claim goes away. So
//     deleting the record here deletes the PersistentVolume *object* and
//     nothing else — the export, the share, the appliance's volume are
//     untouched, and every byte stays where it is.
//   - **`hostPath` is refused, in words.** It is the spike's boundary
//     (docs/HELM-CHARTS.md, "What the platform will not take") and a route
//     for it would be a route around it.
//
// The volume is cluster-scoped and holds no credential, but writing one is
// the operator's: it is bootstrap-adjacent in exactly the way a Connection
// is, and a developer who could mint volumes could name any export on the
// network.
//
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;delete

// volumeAccessModes are the three modes a claim can ask to mount with, and
// so the three this route will write. `ReadWriteOncePod` is deliberately not
// among them: nothing in the claim vocabulary asks for it, and a volume
// offering only that could never be bound.
var volumeAccessModes = map[string]corev1.PersistentVolumeAccessMode{
	string(corev1.ReadOnlyMany):  corev1.ReadOnlyMany,
	string(corev1.ReadWriteOnce): corev1.ReadWriteOnce,
	string(corev1.ReadWriteMany): corev1.ReadWriteMany,
}

var volumeAccessModeNames = []string{
	string(corev1.ReadOnlyMany), string(corev1.ReadWriteOnce), string(corev1.ReadWriteMany),
}

// csiSecretFields are the five references a CSI volume can carry to a Secret
// the driver reads. Each is accepted by the decoder only so that it can be
// refused with a reason — see refuseCSISecrets.
var csiSecretFields = []string{
	"nodePublishSecretRef", "nodeStageSecretRef", "controllerPublishSecretRef",
	"controllerExpandSecretRef", "nodeExpandSecretRef",
}

// credentialWords are what a `volumeAttributes` key is refused for
// containing. CSI attributes are stored verbatim on a cluster-scoped object
// and are read back by every listing of it, so a driver credential pasted
// into one is a credential in cleartext on the least protected object in the
// cluster — which is the one thing this API is built never to hold.
var credentialWords = []string{"secret", "password", "passwd", "token", "credential", "passphrase", "apikey"}

// createPersistentVolumeRequest is the two shapes a home installation
// actually has, and the two the spike found: an NFS export, and a volume a
// storage appliance's own CSI driver hands out.
//
// `hostPath` and `persistentVolumeReclaimPolicy` are fields here so that
// asking for them is answered with the reason rather than with "unknown
// field": a refusal nobody understands is a refusal somebody works around.
type createPersistentVolumeRequest struct {
	Name        string   `json:"name"`
	Capacity    string   `json:"capacity"`
	AccessModes []string `json:"accessModes"`

	NFS *nfsVolumeRequest `json:"nfs,omitempty"`
	CSI *csiVolumeRequest `json:"csi,omitempty"`

	// HostPath is never written. See refuseHostPath.
	HostPath map[string]any `json:"hostPath,omitempty"`
	// ReclaimPolicy is `Retain` or a refusal. It is accepted spelled the way
	// the Kubernetes object spells it, because that is what somebody
	// translating a manifest will type.
	ReclaimPolicy string `json:"persistentVolumeReclaimPolicy,omitempty"`
}

// nfsVolumeRequest is an export on a server: the overwhelming case.
type nfsVolumeRequest struct {
	Server   string `json:"server"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// csiVolumeRequest is a volume a driver already knows about, named by the
// handle that driver gave it.
type csiVolumeRequest struct {
	Driver           string            `json:"driver"`
	VolumeHandle     string            `json:"volumeHandle"`
	FSType           string            `json:"fsType,omitempty"`
	ReadOnly         bool              `json:"readOnly,omitempty"`
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`

	// The five secret references, accepted only to be refused. They are
	// `any` because the refusal is about the field being there at all, and
	// a caller who typed one wrongly should still get the reason rather
	// than a parse error.
	NodePublishSecretRef       any `json:"nodePublishSecretRef,omitempty"`
	NodeStageSecretRef         any `json:"nodeStageSecretRef,omitempty"`
	ControllerPublishSecretRef any `json:"controllerPublishSecretRef,omitempty"`
	ControllerExpandSecretRef  any `json:"controllerExpandSecretRef,omitempty"`
	NodeExpandSecretRef        any `json:"nodeExpandSecretRef,omitempty"`
}

// persistentVolumeView is one volume the platform wrote, as the screen and
// the claim form read it.
//
// It carries what the volume points at — the server and export, the driver
// and handle — because that is the whole of what an operator is checking
// when they read this list: whether the name in front of them is the NAS
// they meant. `reclaimPolicy` is stated rather than implied, since "nothing
// here can delete your data" is the promise the screen is making.
type persistentVolumeView struct {
	Name          string   `json:"name"`
	Capacity      string   `json:"capacity,omitempty"`
	AccessModes   []string `json:"accessModes"`
	Phase         string   `json:"phase,omitempty"`
	ReclaimPolicy string   `json:"reclaimPolicy"`
	// Type is `nfs`, `csi`, or the empty string for a volume written before
	// this route existed and adopted by nothing.
	Type string `json:"type,omitempty"`
	// Identity is what the volume points at, in the same spelling the claim
	// reconciler compares two volumes with: `nfs://server/export`,
	// `csi://driver/handle`.
	Identity string              `json:"identity,omitempty"`
	NFS      *nfsVolumeView      `json:"nfs,omitempty"`
	CSI      *csiVolumeView      `json:"csi,omitempty"`
	HeldBy   []string            `json:"heldBy,omitempty"`
	Created  *metav1.Time        `json:"createdAt,omitempty"`
	Claim    *volumeClaimRefView `json:"claimedBy,omitempty"`
}

type nfsVolumeView struct {
	Server   string `json:"server"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// csiVolumeView carries the attributes back because they are configuration
// and not credentials — the route refuses anything credential-shaped on the
// way in, which is what makes reading them back safe.
type csiVolumeView struct {
	Driver           string            `json:"driver"`
	VolumeHandle     string            `json:"volumeHandle"`
	FSType           string            `json:"fsType,omitempty"`
	ReadOnly         bool              `json:"readOnly,omitempty"`
	VolumeAttributes map[string]string `json:"volumeAttributes,omitempty"`
}

// volumeClaimRefView is the PersistentVolumeClaim the volume is bound to,
// where it is bound to one that is not a Kitchen claim's.
type volumeClaimRefView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type persistentVolumesBody struct {
	Items []persistentVolumeView `json:"items"`
}

// listPersistentVolumes answers with the volumes the platform wrote, and
// with nothing else. A PersistentVolume somebody else made is not this
// route's to report on: it is on `GET /claim-volumes` with everything else
// a claim could bind, and the operator's list here is the list of what the
// platform is accountable for.
func (s *Server) listPersistentVolumes(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	volumes := &corev1.PersistentVolumeList{}
	if err := s.reader().List(ctx, volumes,
		client.MatchingLabels{managedByLabelKey: managedByLabelValue}); err != nil {
		s.writeError(w, err)
		return
	}
	held, err := s.claimsHoldingVolumes(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := persistentVolumesBody{Items: make([]persistentVolumeView, 0, len(volumes.Items))}
	for i := range volumes.Items {
		body.Items = append(body.Items, newPersistentVolumeView(&volumes.Items[i], held))
	}
	sort.Slice(body.Items, func(a, b int) bool { return body.Items[a].Name < body.Items[b].Name })
	writeJSON(w, http.StatusOK, body)
}

// claimsHoldingVolumes is which Kitchen claims currently mount each
// PersistentVolume, as `project/claim`. It is read from the claims' own
// status rather than from the volume's `claimRef`, because the claimRef
// names a PersistentVolumeClaim in an application namespace and the sentence
// an operator needs is the one naming the project.
func (s *Server) claimsHoldingVolumes(ctx context.Context) (map[string][]string, error) {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, claims, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	held := map[string][]string{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		bound := claim.Status.Volume
		if claim.Spec.Type != kitchenv1alpha1.ClaimTypeVolume || bound == nil || bound.Bound == nil {
			continue
		}
		name := bound.Bound.PersistentVolume
		held[name] = append(held[name], claim.Spec.ProjectRef.Name+"/"+claim.Name)
	}
	for name := range held {
		sort.Strings(held[name])
	}
	return held, nil
}

func newPersistentVolumeView(pv *corev1.PersistentVolume, held map[string][]string) persistentVolumeView {
	created := pv.CreationTimestamp
	view := persistentVolumeView{
		Name:          pv.Name,
		Capacity:      quantityOf(pv.Spec.Capacity),
		AccessModes:   accessModeNames(pv.Spec.AccessModes),
		Phase:         string(pv.Status.Phase),
		ReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy),
		Identity:      volume.VolumeIdentity(pv),
		HeldBy:        held[pv.Name],
		Created:       &created,
	}
	switch source := pv.Spec.PersistentVolumeSource; {
	case source.NFS != nil:
		view.Type = "nfs"
		view.NFS = &nfsVolumeView{
			Server: source.NFS.Server, Path: source.NFS.Path, ReadOnly: source.NFS.ReadOnly,
		}
	case source.CSI != nil:
		view.Type = "csi"
		view.CSI = &csiVolumeView{
			Driver:           source.CSI.Driver,
			VolumeHandle:     source.CSI.VolumeHandle,
			FSType:           source.CSI.FSType,
			ReadOnly:         source.CSI.ReadOnly,
			VolumeAttributes: source.CSI.VolumeAttributes,
		}
	}
	if ref := pv.Spec.ClaimRef; ref != nil {
		view.Claim = &volumeClaimRefView{Namespace: ref.Namespace, Name: ref.Name}
	}
	return view
}

// createPersistentVolume writes one PersistentVolume for storage that
// already exists, and refuses everything that would make it something else.
func (s *Server) createPersistentVolume(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := createPersistentVolumeRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)

	if body.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if errs := validation.IsDNS1123Subdomain(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS name — lowercase letters, digits, '-' and '.', starting and "+
			"ending alphanumeric (got %q)", body.Name)
		return
	}
	if refuseHostPath(w, body.HostPath) {
		return
	}
	if body.ReclaimPolicy != "" && body.ReclaimPolicy != string(corev1.PersistentVolumeReclaimRetain) {
		badRequest(w, "persistentVolumeReclaimPolicy is always %s here and cannot be set to %q: this volume "+
			"points at data that existed before the cluster did, and a policy that lets a driver erase it "+
			"when a claim goes away is not one the platform will write. Deleting the volume from here "+
			"removes the record and leaves every byte of it where it is",
			corev1.PersistentVolumeReclaimRetain, body.ReclaimPolicy)
		return
	}

	source, err := volumeSource(&body)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	capacity, err := volumeCapacityOf(body.Capacity)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	modes, err := volumeAccessModesOf(body.AccessModes)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	// Checked before the write so that a name collision cannot repoint a
	// volume something is already mounting.
	existing := &corev1.PersistentVolume{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: body.Name}, existing); err == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"there is already a volume named %q in this cluster", body.Name)})
		return
	} else if !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Labels:      map[string]string{managedByLabelKey: managedByLabelValue},
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: capacity},
			AccessModes:            modes,
			PersistentVolumeSource: source,
			// Retain, always. It is not a default and there is no field that
			// changes it: see the refusal above.
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			// No class. A statically written volume belongs to none, and the
			// claim the reconciler cuts for it copies whatever is here — so
			// naming the cluster's default would produce a claim that never
			// binds to this volume.
			StorageClassName: "",
		},
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    pv,
		Kind:      audit.KindPersistentVolume,
		Operation: clickhouse.AuditCreate,
		To:        identityOf(&body),
		Reason: fmt.Sprintf("volume %s written for %s, reclaim policy %s", body.Name, identityOf(&body),
			corev1.PersistentVolumeReclaimRetain),
		Details: map[string]any{"identity": identityOf(&body), "capacity": body.Capacity},
	}) {
		return
	}
	if err := s.Client.Create(ctx, pv); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("persistent volume written through the api",
		"volume", pv.Name, "identity", identityOf(&body), "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newPersistentVolumeView(pv, nil))
}

// deletePersistentVolume removes a volume the platform wrote — the object
// and nothing else.
//
// Two refusals stand in front of it. A volume the platform did not write is
// answered as though it were not there, because this route's whole subject
// is what the platform is accountable for and adopting somebody's
// hand-written manifest by deleting it is the opposite of that. A volume a
// claim is currently mounting is refused naming the claim, because the
// alternative is a project whose pods stop being able to start and a
// sentence nobody can trace back.
func (s *Server) deletePersistentVolume(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := req.PathValue("name")

	pv := &corev1.PersistentVolume{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, pv); err != nil {
		s.writeError(w, err)
		return
	}
	if pv.Labels[managedByLabelKey] != managedByLabelValue {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"the platform did not write the volume %q, so it is not the platform's to remove: it holds only "+
				"what it created, and something else on this cluster owns that object", name)})
		return
	}

	held, err := s.claimsHoldingVolumes(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if holders := held[pv.Name]; len(holders) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"volume %q is mounted by %s: delete the claim first, and the data stays where it is either way",
			pv.Name, strings.Join(holders, ", "))})
		return
	}
	// Bound to something outside the platform — a claim written another way,
	// or one whose project is gone. It is still a mount, and the two names
	// are what somebody needs to go and look.
	if ref := pv.Spec.ClaimRef; ref != nil && pv.Status.Phase == corev1.VolumeBound {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"volume %q is bound to the claim %s/%s, which is not one of this platform's: whatever mounts it "+
				"would lose the mount", pv.Name, ref.Namespace, ref.Name)})
		return
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    pv,
		Kind:      audit.KindPersistentVolume,
		Operation: clickhouse.AuditDelete,
		From:      volume.VolumeIdentity(pv),
		Reason: fmt.Sprintf("volume %s removed; %s reclaim policy leaves the data where it is", pv.Name,
			pv.Spec.PersistentVolumeReclaimPolicy),
		Details: map[string]any{"identity": volume.VolumeIdentity(pv),
			"reclaimPolicy": string(pv.Spec.PersistentVolumeReclaimPolicy)},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("persistent volume deleted through the api", "volume", pv.Name, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// refuseHostPath answers a request that reaches for a node's filesystem, and
// answers it with the reasoning rather than with a bare 400. It is the
// spike's boundary: a project pinned to one machine's disk cannot move,
// cannot preview and cannot scale, and the platform would be lying about all
// three.
func refuseHostPath(w http.ResponseWriter, hostPath map[string]any) bool {
	if hostPath == nil {
		return false
	}
	badRequest(w, "hostPath is refused: a volume that is a directory on one machine ties whatever mounts it "+
		"to that machine, which cannot move, cannot be previewed and cannot scale — and the premise this "+
		"platform is built on is that the cluster is abstracted away. It is a boundary rather than a missing "+
		"field (docs/HELM-CHARTS.md, \"What the platform will not take\"). Export the directory over NFS and "+
		"write an nfs volume for it, or attach it through a CSI driver")
	return true
}

// volumeSource turns the request into the one source it names, and refuses
// naming none or both. The two shapes are the two the spike found: the NFS
// export a home installation already has, and the volume a storage
// appliance's own driver hands out.
func volumeSource(body *createPersistentVolumeRequest) (corev1.PersistentVolumeSource, error) {
	switch {
	case body.NFS != nil && body.CSI != nil:
		return corev1.PersistentVolumeSource{}, fmt.Errorf(
			"a volume is nfs or csi, not both: one object points at one thing")
	case body.NFS != nil:
		server := strings.TrimSpace(body.NFS.Server)
		export := strings.TrimSpace(body.NFS.Path)
		if server == "" {
			return corev1.PersistentVolumeSource{}, fmt.Errorf(
				"nfs.server is required: the host that serves the export, e.g. \"nas.lan\"")
		}
		if export == "" || !strings.HasPrefix(export, "/") {
			return corev1.PersistentVolumeSource{}, fmt.Errorf(
				"nfs.path is the exported path on the server and is absolute, e.g. \"/export/media\" (got %q)",
				body.NFS.Path)
		}
		return corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{
			Server: server, Path: path.Clean(export), ReadOnly: body.NFS.ReadOnly,
		}}, nil
	case body.CSI != nil:
		return csiVolumeSource(body.CSI)
	default:
		return corev1.PersistentVolumeSource{}, fmt.Errorf(
			"a volume names what it points at: an nfs block (server, path) or a csi block (driver, " +
				"volumeHandle)")
	}
}

func csiVolumeSource(csi *csiVolumeRequest) (corev1.PersistentVolumeSource, error) {
	driver := strings.TrimSpace(csi.Driver)
	handle := strings.TrimSpace(csi.VolumeHandle)
	if driver == "" {
		return corev1.PersistentVolumeSource{}, fmt.Errorf(
			"csi.driver is required: the name the driver registers under, e.g. \"csi.truenas.net\"")
	}
	if handle == "" {
		return corev1.PersistentVolumeSource{}, fmt.Errorf(
			"csi.volumeHandle is required: the id the driver knows this volume by")
	}
	if err := refuseCSISecrets(csi); err != nil {
		return corev1.PersistentVolumeSource{}, err
	}
	for key := range csi.VolumeAttributes {
		lower := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		for _, word := range credentialWords {
			if strings.Contains(lower, word) {
				return corev1.PersistentVolumeSource{}, fmt.Errorf(
					"csi.volumeAttributes.%s reads as a credential, and attributes are written to the volume "+
						"in cleartext where every reader of it can see them. The platform holds credentials "+
						"in secrets it never reads back, and it will not write one here", key)
			}
		}
	}
	return corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
		Driver:           driver,
		VolumeHandle:     handle,
		FSType:           strings.TrimSpace(csi.FSType),
		ReadOnly:         csi.ReadOnly,
		VolumeAttributes: csi.VolumeAttributes,
	}}, nil
}

// refuseCSISecrets keeps every reference to a Secret off the volumes this
// route writes.
//
// A reference is not itself credential material, so this is a boundary
// rather than a leak being closed: the Secret it would name is one the
// platform never wrote and cannot account for, and pointing a cluster-scoped
// object the platform owns at it makes the platform responsible for a
// credential it has never seen. A driver that cannot mount without one is
// not expressible here yet, and saying so is better than half-writing it.
func refuseCSISecrets(csi *csiVolumeRequest) error {
	present := []string{}
	for i, field := range []any{
		csi.NodePublishSecretRef, csi.NodeStageSecretRef, csi.ControllerPublishSecretRef,
		csi.ControllerExpandSecretRef, csi.NodeExpandSecretRef,
	} {
		if field != nil {
			present = append(present, "csi."+csiSecretFields[i])
		}
	}
	if len(present) == 0 {
		return nil
	}
	return fmt.Errorf("%s is refused: it points the volume at a credential this platform did not write and "+
		"cannot account for, and a volume the platform owns must not depend on one. A driver that cannot "+
		"mount without a secret is not expressible here", strings.Join(present, ", "))
}

func volumeCapacityOf(raw string) (resource.Quantity, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return resource.Quantity{}, fmt.Errorf(
			"capacity is required: how much of the storage this volume offers, e.g. \"12Ti\". It is what a " +
				"claim mounting it asks for, and nothing enforces it against the export itself")
	}
	quantity, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("capacity is a Kubernetes quantity like \"500Gi\" or \"12Ti\" (got %q)", raw)
	}
	if quantity.Sign() <= 0 {
		return resource.Quantity{}, fmt.Errorf("capacity is more than zero (got %q)", raw)
	}
	return quantity, nil
}

func volumeAccessModesOf(names []string) ([]corev1.PersistentVolumeAccessMode, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("accessModes is required: what this storage can do, one or more of %s. It is "+
			"what a claim's own mode is checked against", strings.Join(volumeAccessModeNames, ", "))
	}
	modes := make([]corev1.PersistentVolumeAccessMode, 0, len(names))
	seen := map[corev1.PersistentVolumeAccessMode]bool{}
	for _, name := range names {
		mode, ok := volumeAccessModes[strings.TrimSpace(name)]
		if !ok {
			return nil, fmt.Errorf("accessModes carries %q, which is not one of %s", name,
				strings.Join(volumeAccessModeNames, ", "))
		}
		if seen[mode] {
			continue
		}
		seen[mode] = true
		modes = append(modes, mode)
	}
	return modes, nil
}

// identityOf is what the request points at, in the spelling the claim
// reconciler compares two volumes with — so the audit record and the claim's
// status say the same thing about the same storage.
func identityOf(body *createPersistentVolumeRequest) string {
	switch {
	case body.NFS != nil:
		return fmt.Sprintf("nfs://%s%s", strings.TrimSpace(body.NFS.Server),
			path.Clean("/"+strings.TrimSpace(body.NFS.Path)))
	case body.CSI != nil:
		return fmt.Sprintf("csi://%s/%s", strings.TrimSpace(body.CSI.Driver),
			strings.TrimSpace(body.CSI.VolumeHandle))
	}
	return ""
}

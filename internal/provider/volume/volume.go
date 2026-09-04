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

// Package volume is the contract of the volume claim type: a persistent
// volume mounted into one of a project's processes, for the workload that
// must write to a filesystem — a legacy application, SQLite, anything that
// writes where it was told to write rather than where a cloud-native rewrite
// would put it.
//
// It is the odd contract, and deliberately so: every other one produces
// credentials, and this one produces a mount. There is no Connection and no
// provisioner interface — the provider is the cluster's StorageClass, which
// Kitchen requires of every cluster anyway, and the reconciler creates a
// PersistentVolumeClaim from it — so what lives here is what every contract
// has to say about itself: who the provider is, what a claim asks for, and
// what it declares about previews and about the workload that reads it.
//
// The declaration is the point. A volume is ReadWriteOnce unless the class
// genuinely supports otherwise, and ReadWriteOnce does two things to the
// workload that are not fine if they are discovered later: it caps it at
// one replica, and it forces strategy Recreate — a rolling update creates the
// new pod before terminating the old one, both want the same volume, and the
// new one sits in Multi-Attach while the Deployment waits for it to become
// ready before killing the old one. That is a deadlock, not a delay, and
// Recreate resolves it at the cost of downtime on every deploy. Where the
// class supports ReadWriteMany, detected and never assumed, both are lifted.
package volume

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Bermos/Kitchen/internal/provider/contract"
)

// ProviderName is who declares a volume claim's data provenance and its
// preview mode: the cluster's StorageClass, since there is no Connection
// whose provider could say instead.
const ProviderName = "storageClass"

// BoundProviderName is the other provider of a volume claim: a volume that
// already exists, which the platform mounts and never cuts.
//
// It is a second provider rather than a flag on the first because
// everything a provider declares differs between them — what a preview
// gets, what deleting the claim may do, whether there is a size to ask for
// — and a declaration is what the dashboard shows and the docs matrix
// renders before any claim exists. A reader choosing between "cut me a
// disk" and "mount the NAS" is choosing between two providers, and the
// catalogue says so.
const BoundProviderName = "boundVolume"

const (
	// ReadWriteManyAnnotation on a StorageClass is an operator's word about
	// whether volumes cut from it can be attached to more than one node at
	// once: "true" or "false". Kubernetes records nothing of the kind on a
	// class — access modes are a property of the provisioner behind it —
	// so this is how a class the allowlist below does not know is declared
	// shared, and how one it does know is declared not to be.
	ReadWriteManyAnnotation = "kitchen.bermos.dev/read-write-many"

	// DefaultClassAnnotation marks the cluster's default StorageClass, as
	// Kubernetes spells it.
	DefaultClassAnnotation = "storageclass.kubernetes.io/is-default-class"
)

// readWriteManyProvisioners are the CSI drivers whose volumes are shared
// filesystems by construction — NFS, SMB and the three clouds' file
// services, plus CephFS — and so support ReadWriteMany from every class.
// Everything else is treated as ReadWriteOnce unless its class says
// otherwise with the annotation: a block driver that can do both (Longhorn
// over NFS, say) is a class-level configuration an operator knows about and
// the platform cannot see.
//
// Matched as a suffix, because Ceph's provisioners are prefixed with the
// cluster they belong to: rook-ceph.cephfs.csi.ceph.com.
var readWriteManyProvisioners = []string{
	"nfs.csi.k8s.io",
	"smb.csi.k8s.io",
	"efs.csi.aws.com",
	"file.csi.azure.com",
	"filestore.csi.storage.gke.io",
	"cephfs.csi.ceph.com",
}

// Source is where a volume claim's volume comes from. It is declared on the
// claim rather than inferred from which fields are set: "the claim has a
// bind block, so it must mean bind" is a guess, and a guess about whether
// the platform is about to cut a new disk or reach for twelve terabytes
// somebody already has is the wrong kind of guess.
type Source string

const (
	// SourceProvision cuts a new PersistentVolumeClaim from a StorageClass:
	// the whole of what a volume claim was when it arrived.
	SourceProvision Source = "provision"
	// SourceBind mounts a volume that already exists — a PersistentVolume
	// an operator wrote for an NFS export or a CSI volume, or a
	// PersistentVolumeClaim already sitting in the project's application
	// namespace. The platform provisions nothing and destroys nothing.
	SourceBind Source = "bind"
)

// Sources is every source, in the order the docs list them.
var Sources = []Source{SourceProvision, SourceBind}

// Binding is the volume a claim binds when its source is bind: which volume,
// and how it is mounted.
type Binding struct {
	// PersistentVolume names a cluster PersistentVolume to bind. Exactly
	// one of this and PersistentVolumeClaim is set.
	PersistentVolume string
	// PersistentVolumeClaim names an existing PersistentVolumeClaim in the
	// project's own application namespace — one somebody put there, or one
	// an earlier life of the project left behind.
	//
	// It is the project's namespace and no other, because that is the only
	// namespace whose claims its pods can mount: a PersistentVolumeClaim is
	// namespaced and a pod may only name one of its own. A volume that
	// lives somewhere else is reached by naming the PersistentVolume behind
	// it, which is cluster-scoped and is what an export is anyway.
	PersistentVolumeClaim string
	// AccessMode is how this project mounts the volume — declared, not
	// read off the volume, because "what the volume can do" and "what this
	// project may do with it" are different questions and only the second
	// one decides whether another project may write it.
	AccessMode corev1.PersistentVolumeAccessMode
}

// AccessModes are the three modes a bound volume may be declared with, in
// the order the docs list them.
var AccessModes = []corev1.PersistentVolumeAccessMode{
	corev1.ReadOnlyMany, corev1.ReadWriteOnce, corev1.ReadWriteMany,
}

// Writes reports whether a mode lets the process that mounts the volume
// change what is on it. It is the whole of the two-projects-one-volume
// rule: any number of projects may read one volume, and one may write it.
func Writes(mode corev1.PersistentVolumeAccessMode) bool {
	return mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteMany
}

// AttachesOnce reports whether a volume mounted this way can be attached to
// one pod at a time — which is what caps the process at one replica and
// deploys it by recreation. ReadOnlyMany and ReadWriteMany both lift it.
func AttachesOnce(mode corev1.PersistentVolumeAccessMode) bool {
	return mode != corev1.ReadWriteMany && mode != corev1.ReadOnlyMany
}

// Requirements is what a volume claim asks for, read off its config: which
// process mounts the volume, where, and what it has to be — or, for a bound
// volume, which volume it already is.
type Requirements struct {
	// Process is the project's process that mounts the volume — "web" for
	// the web process, or a named process.
	Process string
	// Source is provision or bind. Empty is provision, which is what every
	// claim written before binding existed meant.
	Source Source
	// Size is a Kubernetes quantity: "10Gi". Provisioned volumes only.
	Size string
	// StorageClass the volume is cut from; empty is the cluster's default.
	// Provisioned volumes only.
	StorageClass string
	// MountPath is where the volume appears in the process's container.
	MountPath string
	// Bind is the volume a bound claim mounts.
	Bind Binding
}

// Bound reports whether the claim mounts a volume that already exists.
func (r Requirements) Bound() bool { return r.Source == SourceBind }

// Validate checks the shape of the requirements — and only the shape.
// Whether the process exists is the project's answer, and whether the class
// or the volume exists is the cluster's; all of them land on the claim.
func (r Requirements) Validate() error {
	if r.Process == "" {
		return errors.New("volume.process is required: it names the one process that mounts the volume — " +
			"\"web\" for the web process, or one of the project's processes — because a volume can be " +
			"attached to one pod at a time, and every pod an environment runs would otherwise want it")
	}
	if errs := validation.IsDNS1123Label(r.Process); len(errs) > 0 {
		return fmt.Errorf("volume.process must be a process name — lowercase letters, digits and '-' (got %q)",
			r.Process)
	}
	if err := r.validateSource(); err != nil {
		return err
	}
	if r.MountPath == "" {
		return errors.New("volume.mountPath is required: the absolute path inside the container the volume " +
			"appears at, such as \"/data\"")
	}
	if !path.IsAbs(r.MountPath) || path.Clean(r.MountPath) != r.MountPath || r.MountPath == "/" {
		return fmt.Errorf("volume.mountPath must be an absolute, clean path below the root — \"/data\", not %q: "+
			"mounting over the root would hide the image's own filesystem", r.MountPath)
	}
	return nil
}

// validateSource checks the half of the requirements that differs between
// the two sources, and — first — that each source was given only its own
// fields. A size on a bound volume is not a harmless extra: it reads as a
// disk about to be cut at that size, and nothing is about to be cut.
func (r Requirements) validateSource() error {
	switch r.Source {
	case "", SourceProvision:
		if r.Bind != (Binding{}) {
			return fmt.Errorf("volume.bind is refused on a %s volume: it names a volume that already exists, "+
				"and this claim is asking the platform to cut a new one. Set volume.source: %s to mount the "+
				"volume it names, or take volume.bind off", SourceProvision, SourceBind)
		}
		if r.Size == "" {
			return errors.New("volume.size is required: a Kubernetes quantity such as \"10Gi\". A volume has " +
				"no sensible default size, and one that was defaulted is the one that fills up first")
		}
		quantity, err := resource.ParseQuantity(r.Size)
		if err != nil {
			return fmt.Errorf("volume.size is a Kubernetes quantity — \"10Gi\" (got %q): %w", r.Size, err)
		}
		if quantity.Sign() <= 0 {
			return fmt.Errorf("volume.size must be more than nothing (got %q)", r.Size)
		}
		if r.StorageClass != "" {
			if errs := validation.IsDNS1123Subdomain(r.StorageClass); len(errs) > 0 {
				return fmt.Errorf("volume.storageClass must be a StorageClass name (got %q): %s",
					r.StorageClass, strings.Join(errs, "; "))
			}
		}
		return nil
	case SourceBind:
		if r.Size != "" {
			return fmt.Errorf("volume.size is refused on a %s volume: the volume already exists and the "+
				"platform is not provisioning it, so a size here would be a number nothing reads. Its "+
				"capacity is whatever it was made with, and the claim reports it", SourceBind)
		}
		if r.StorageClass != "" {
			return fmt.Errorf("volume.storageClass is refused on a %s volume: a class is what a volume is cut "+
				"from, and this one was cut before the claim existed. The claim reports the class the volume "+
				"carries, if it carries one", SourceBind)
		}
		return r.Bind.validate()
	}
	return fmt.Errorf("volume.source must be %s (got %q): %s cuts a new volume from a StorageClass, %s mounts "+
		"one that already exists", joinSources(), r.Source, SourceProvision, SourceBind)
}

// validate checks the bind block: exactly one volume named, and an access
// mode that was written down.
func (b Binding) validate() error {
	named := 0
	if b.PersistentVolume != "" {
		named++
	}
	if b.PersistentVolumeClaim != "" {
		named++
	}
	switch named {
	case 0:
		return fmt.Errorf("volume.bind names no volume: set bind.persistentVolume to the PersistentVolume the "+
			"volume is (an NFS export or a CSI volume an operator wrote), or bind.persistentVolumeClaim to a "+
			"PersistentVolumeClaim already in this project's namespace. A %s volume mounts one that exists "+
			"and creates none", SourceBind)
	case 1:
	default:
		return errors.New("volume.bind names both a persistentVolume and a persistentVolumeClaim: name one. " +
			"A PersistentVolumeClaim already names the volume behind it, so the two together can only agree " +
			"or contradict each other")
	}
	for _, name := range []struct{ field, value string }{
		{"bind.persistentVolume", b.PersistentVolume},
		{"bind.persistentVolumeClaim", b.PersistentVolumeClaim},
	} {
		if name.value == "" {
			continue
		}
		if errs := validation.IsDNS1123Subdomain(name.value); len(errs) > 0 {
			return fmt.Errorf("volume.%s must be a Kubernetes object name (got %q): %s", name.field, name.value,
				strings.Join(errs, "; "))
		}
	}
	if b.AccessMode == "" {
		return fmt.Errorf("volume.bind.accessMode is required: it says how this project mounts the volume — "+
			"%s to read it, %s to write it as the only pod that has it, %s to write it alongside others. It "+
			"is declared rather than read off the volume because what the volume can do and what this "+
			"project may do with it are different questions", corev1.ReadOnlyMany, corev1.ReadWriteOnce,
			corev1.ReadWriteMany)
	}
	if !slices.Contains(AccessModes, b.AccessMode) {
		return fmt.Errorf("volume.bind.accessMode must be one of %s (got %q)", joinModes(), b.AccessMode)
	}
	return nil
}

func joinSources() string {
	names := make([]string, 0, len(Sources))
	for _, source := range Sources {
		names = append(names, string(source))
	}
	return strings.Join(names, " or ")
}

func joinModes() string {
	names := make([]string, 0, len(AccessModes))
	for _, mode := range AccessModes {
		names = append(names, string(mode))
	}
	return strings.Join(names, ", ")
}

// Declaration is what the StorageClass provider says about the volumes it
// cuts.
//
// A preview gets a fresh, empty volume of its own — the same size and class,
// none of production's data — created with the preview and deleted with it,
// which is what CloudNativePG already does for preview databases and for the
// same reason: there is no copy-on-write branch of a block device, and a copy
// of production's disk under a pull request is both slow and production data
// in a preview.
//
// And it forces a recreate, always: the declaration is a fact about the
// provider, and the provider is a StorageClass whose access mode the platform
// cannot know until it has looked. The reconciler looks, and where the class
// turns out to support ReadWriteMany it writes the lifted constraint on the
// claim's status over this declaration. What the screen shows before the
// claim exists is the conservative answer, which is the right one to show.
var Declaration = contract.Declaration{
	Preview: contract.PreviewFresh,
	PreviewNote: "a new, empty volume of the same size and class, never a copy of production's: the preview " +
		"declares provenance synthetic",
	IdleNote: "a PersistentVolumeClaim is storage and no compute, so there is nothing to park: an idle " +
		"preview's volume costs its capacity and nothing else",
	ForcesRecreate: true,
	WorkloadNote: "a ReadWriteOnce volume attaches to one pod at a time, so the process mounting it runs one " +
		"replica and is deployed by stopping the old pod before starting the new one — a rolling update " +
		"would leave the new pod waiting in Multi-Attach for a volume the old pod never releases. " +
		"Every deploy of that process has a gap in serving; a StorageClass detected to support " +
		"ReadWriteMany lifts both",
}

// BoundDeclaration is what the other provider says: the volume already
// exists, so there is nothing to cut, nothing to copy and nothing the
// platform may destroy.
//
// **What a preview gets is the one thing this had to decide.** A fresh,
// empty volume is what the provisioning provider gives a preview, and it is
// exactly wrong here: an existing volume is usually the whole point of the
// application — a preview of a media server with an empty media directory
// is a preview of nothing. So a preview mounts the same volume, read-only,
// and that is a default rather than a choice to opt into because a
// read-only mount can neither take the volume from production nor change
// what is on it, which is what the refusal on `shared` exists for
// (SharedIsReadOnly). Where the claim's own access mode is ReadWriteOnce
// the volume attaches to one pod at a time and production has it: previews
// get nothing, and the claim says so.
//
// ForcesRecreate is declared true because the conservative answer is the
// right one to show before a claim exists — the claim's declared access
// mode is what actually decides, and the reconciler writes that over this.
var BoundDeclaration = contract.Declaration{
	Preview: contract.PreviewShared,
	PreviewNote: "the same volume, mounted read-only: a preview of an application whose data is the point of " +
		"it reads exactly what production reads and cannot change any of it. A ReadWriteOnce volume gives " +
		"previews nothing instead — production has it, and it attaches to one pod at a time",
	SharedIsReadOnly: true,
	IdleNote: "the volume is not the platform's to park: it existed before the claim and outlives it, and an " +
		"idle preview mounting it read-only costs nothing either way",
	ForcesRecreate: true,
	WorkloadNote: "a volume mounted ReadWriteOnce attaches to one pod at a time, so the process mounting it " +
		"runs one replica and is deployed by stopping the old pod before starting the new one. The claim " +
		"declares its own access mode, and ReadOnlyMany or ReadWriteMany lifts both",
}

// VolumeIdentity is what a PersistentVolume actually points at, as a string
// two claims can be compared on: the CSI driver and volume handle, the NFS
// server and export, the SMB share, and so on down to the volume's own name
// where the platform cannot see inside it.
//
// It exists for one rule. Two projects mounting one export do it through
// two PersistentVolumes — a PersistentVolume binds to exactly one claim, so
// there is no other way — and comparing the names would report two
// unrelated volumes where the storage underneath is the same twelve
// terabytes. What the claim has to know is whether somebody else is writing
// where this claim is about to write, and that is a question about the
// storage, not about the object in front of it.
func VolumeIdentity(pv *corev1.PersistentVolume) string {
	if pv == nil {
		return ""
	}
	switch source := pv.Spec.PersistentVolumeSource; {
	case source.CSI != nil:
		return fmt.Sprintf("csi://%s/%s", source.CSI.Driver, source.CSI.VolumeHandle)
	case source.NFS != nil:
		return fmt.Sprintf("nfs://%s%s", source.NFS.Server, path.Clean("/"+source.NFS.Path))
	case source.ISCSI != nil:
		return fmt.Sprintf("iscsi://%s/%s/%d", source.ISCSI.TargetPortal, source.ISCSI.IQN, source.ISCSI.Lun)
	case source.FC != nil:
		return fmt.Sprintf("fc://%s", strings.Join(source.FC.TargetWWNs, ","))
	case source.HostPath != nil:
		return "hostPath://" + path.Clean(source.HostPath.Path)
	case source.Local != nil:
		return "local://" + path.Clean(source.Local.Path)
	}
	return "persistentVolume://" + pv.Name
}

// AccessMode is what volumes from a StorageClass can be attached as, and
// the sentence that decided it. ReadWriteMany is answered only on evidence
// — the class's annotation, or a provisioner known to be a shared
// filesystem — and ReadWriteOnce otherwise, because the cost of assuming
// wrongly is a rollout that never finishes.
func AccessMode(class *storagev1.StorageClass) (corev1.PersistentVolumeAccessMode, string) {
	if class == nil {
		return corev1.ReadWriteOnce, "no StorageClass to read, so the volume is treated as attachable to one pod " +
			"at a time"
	}
	switch strings.ToLower(strings.TrimSpace(class.Annotations[ReadWriteManyAnnotation])) {
	case "true":
		return corev1.ReadWriteMany, fmt.Sprintf("StorageClass %s is annotated %s=true, so its volumes can be "+
			"attached to more than one pod at once", class.Name, ReadWriteManyAnnotation)
	case "false":
		return corev1.ReadWriteOnce, fmt.Sprintf("StorageClass %s is annotated %s=false, so its volumes attach to "+
			"one pod at a time", class.Name, ReadWriteManyAnnotation)
	}
	for _, provisioner := range readWriteManyProvisioners {
		if class.Provisioner == provisioner || strings.HasSuffix(class.Provisioner, "."+provisioner) {
			return corev1.ReadWriteMany, fmt.Sprintf("StorageClass %s is provisioned by %s, a shared filesystem, "+
				"so its volumes can be attached to more than one pod at once", class.Name, class.Provisioner)
		}
	}
	return corev1.ReadWriteOnce, fmt.Sprintf("StorageClass %s is provisioned by %s, which is not known to serve "+
		"shared filesystems, so its volumes attach to one pod at a time. Annotate the class %s=true if it does",
		class.Name, class.Provisioner, ReadWriteManyAnnotation)
}

// DefaultClass is the cluster's default StorageClass among the given ones,
// or nil where there is none. Where several are marked default — which
// Kubernetes admits — the newest wins, because that is the one the API
// server itself would put on a claim that named none.
func DefaultClass(classes []storagev1.StorageClass) *storagev1.StorageClass {
	defaults := make([]*storagev1.StorageClass, 0, 1)
	for i := range classes {
		if strings.EqualFold(strings.TrimSpace(classes[i].Annotations[DefaultClassAnnotation]), "true") {
			defaults = append(defaults, &classes[i])
		}
	}
	if len(defaults) == 0 {
		return nil
	}
	sort.SliceStable(defaults, func(a, b int) bool {
		return defaults[a].CreationTimestamp.After(defaults[b].CreationTimestamp.Time)
	})
	return defaults[0]
}

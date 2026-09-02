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

// Requirements is what a volume claim asks for, read off its config: which
// process mounts the volume, where, and what it has to be.
type Requirements struct {
	// Process is the project's process that mounts the volume — "web" for
	// the web process, or a named process.
	Process string
	// Size is a Kubernetes quantity: "10Gi".
	Size string
	// StorageClass the volume is cut from; empty is the cluster's default.
	StorageClass string
	// MountPath is where the volume appears in the process's container.
	MountPath string
}

// Validate checks the shape of the requirements — and only the shape.
// Whether the process exists is the project's answer, and whether the class
// exists and what it supports is the cluster's; both land on the claim.
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
	if r.Size == "" {
		return errors.New("volume.size is required: a Kubernetes quantity such as \"10Gi\". A volume has no " +
			"sensible default size, and one that was defaulted is the one that fills up first")
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
			return fmt.Errorf("volume.storageClass must be a StorageClass name (got %q): %s", r.StorageClass,
				strings.Join(errs, "; "))
		}
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
	ForcesRecreate: true,
	WorkloadNote: "a ReadWriteOnce volume attaches to one pod at a time, so the process mounting it runs one " +
		"replica and is deployed by stopping the old pod before starting the new one — a rolling update " +
		"would leave the new pod waiting in Multi-Attach for a volume the old pod never releases. " +
		"Every deploy of that process has a gap in serving; a StorageClass detected to support " +
		"ReadWriteMany lifts both",
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

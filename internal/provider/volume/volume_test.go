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

package volume

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/provider/contract"
)

func TestRequirementsValidateTheShape(t *testing.T) {
	good := Requirements{Process: "web", Size: "10Gi", MountPath: "/data"}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed request must pass: %v", err)
	}
	for _, testCase := range []struct {
		name string
		req  Requirements
		says string
	}{
		{"no process", Requirements{Size: "1Gi", MountPath: "/data"}, "volume.process is required"},
		{"a process that is not a name", Requirements{Process: "Web Server", Size: "1Gi", MountPath: "/data"},
			"volume.process must be a process name"},
		{"no size", Requirements{Process: "web", MountPath: "/data"}, "volume.size is required"},
		{"a size that is not a quantity", Requirements{Process: "web", Size: "ten gigs", MountPath: "/data"},
			"Kubernetes quantity"},
		{"a size of nothing", Requirements{Process: "web", Size: "0", MountPath: "/data"}, "more than nothing"},
		{"a class that is not a name", Requirements{Process: "web", Size: "1Gi", StorageClass: "Fast SSD", MountPath: "/data"},
			"StorageClass name"},
		{"no mount path", Requirements{Process: "web", Size: "1Gi"}, "volume.mountPath is required"},
		{"a relative mount path", Requirements{Process: "web", Size: "1Gi", MountPath: "data"}, "absolute"},
		{"the root", Requirements{Process: "web", Size: "1Gi", MountPath: "/"}, "absolute"},
		{"an unclean path", Requirements{Process: "web", Size: "1Gi", MountPath: "/data/../etc"}, "absolute"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.req.Validate()
			if err == nil {
				t.Fatal("must be refused")
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, err.Error())
			}
		})
	}
}

func TestTheDeclarationIsConservative(t *testing.T) {
	if Declaration.Preview != contract.PreviewFresh {
		t.Errorf("a preview gets a fresh, empty volume; declared %q", Declaration.Preview)
	}
	if !Declaration.ForcesRecreate || Declaration.WorkloadNote == "" {
		t.Error("a volume forces a recreate until the class is known to do better, and says why")
	}
	if Declaration.KeepsPodsRunning {
		t.Error("a volume does not stop an environment idling: 0 → 1 → 0 never has two attachers")
	}
}

func class(name, provisioner string, annotations map[string]string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Annotations: annotations},
		Provisioner: provisioner,
	}
}

func TestAccessModeIsDetectedNeverAssumed(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		class *storagev1.StorageClass
		want  corev1.PersistentVolumeAccessMode
		says  string
	}{
		{"no class at all", nil, corev1.ReadWriteOnce, "no StorageClass"},
		{"a block driver", class("fast", "ebs.csi.aws.com", nil), corev1.ReadWriteOnce, "not known to serve"},
		{"a shared filesystem driver", class("shared", "nfs.csi.k8s.io", nil), corev1.ReadWriteMany, "shared filesystem"},
		{"a prefixed ceph driver", class("cephfs", "rook-ceph.cephfs.csi.ceph.com", nil), corev1.ReadWriteMany,
			"shared filesystem"},
		{"a block driver with the rbd suffix is not cephfs", class("rbd", "rook-ceph.rbd.csi.ceph.com", nil),
			corev1.ReadWriteOnce, "not known"},
		{"the annotation says yes", class("longhorn-nfs", "driver.longhorn.io",
			map[string]string{ReadWriteManyAnnotation: "true"}), corev1.ReadWriteMany, "annotated"},
		{"the annotation overrides the allowlist", class("nfs-ro", "nfs.csi.k8s.io",
			map[string]string{ReadWriteManyAnnotation: "false"}), corev1.ReadWriteOnce, "annotated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mode, reason := AccessMode(testCase.class)
			if mode != testCase.want {
				t.Fatalf("want %s, got %s (%s)", testCase.want, mode, reason)
			}
			if !strings.Contains(reason, testCase.says) {
				t.Fatalf("the reason does not say %q: %q", testCase.says, reason)
			}
		})
	}
}

func TestDefaultClassIsTheNewestMarkedDefault(t *testing.T) {
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-time.Hour))
	classes := []storagev1.StorageClass{
		{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "old-default", CreationTimestamp: older,
			Annotations: map[string]string{DefaultClassAnnotation: "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "new-default", CreationTimestamp: now,
			Annotations: map[string]string{DefaultClassAnnotation: "true"}}},
	}
	if got := DefaultClass(classes); got == nil || got.Name != "new-default" {
		t.Errorf("the newest default wins, as it does at the API server; got %v", got)
	}
	if DefaultClass(classes[:1]) != nil {
		t.Error("a cluster with no default has none")
	}
}

// The bound source is refused the provisioning one's fields and vice versa,
// which is what makes the source a declaration rather than something worked
// out from the shape of a request.
func TestEachSourceIsRefusedTheOthersFields(t *testing.T) {
	good := Requirements{
		Process: "web", Source: SourceBind, MountPath: "/media",
		Bind: Binding{PersistentVolume: "nas-media", AccessMode: corev1.ReadOnlyMany},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a well-formed binding must pass: %v", err)
	}
	byClaim := good
	byClaim.Bind = Binding{PersistentVolumeClaim: "old-data", AccessMode: corev1.ReadWriteOnce}
	if err := byClaim.Validate(); err != nil {
		t.Fatalf("a claim already in the project's namespace is the other way in: %v", err)
	}

	for _, testCase := range []struct {
		name string
		req  Requirements
		says string
	}{
		{"a size on a volume nothing is cutting",
			Requirements{Process: "web", Source: SourceBind, Size: "1Gi", MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas", AccessMode: corev1.ReadOnlyMany}},
			"volume.size is refused on a bind volume"},
		{"a class on a volume nothing is cutting",
			Requirements{Process: "web", Source: SourceBind, StorageClass: "fast", MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas", AccessMode: corev1.ReadOnlyMany}},
			"volume.storageClass is refused"},
		{"a binding on a volume being cut",
			Requirements{Process: "web", Size: "1Gi", MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas", AccessMode: corev1.ReadOnlyMany}},
			"volume.bind is refused on a provision volume"},
		{"naming no volume",
			Requirements{Process: "web", Source: SourceBind, MountPath: "/m",
				Bind: Binding{AccessMode: corev1.ReadOnlyMany}},
			"names no volume"},
		{"naming both",
			Requirements{Process: "web", Source: SourceBind, MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas", PersistentVolumeClaim: "old", AccessMode: corev1.ReadOnlyMany}},
			"names both"},
		{"no access mode",
			Requirements{Process: "web", Source: SourceBind, MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas"}},
			"volume.bind.accessMode is required"},
		{"an access mode that is none of the three",
			Requirements{Process: "web", Source: SourceBind, MountPath: "/m",
				Bind: Binding{PersistentVolume: "nas", AccessMode: "ReadWriteOncePod"}},
			"must be one of"},
		{"a source that is neither",
			Requirements{Process: "web", Source: "borrow", MountPath: "/m"},
			"volume.source must be"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.req.Validate()
			if err == nil {
				t.Fatal("must be refused")
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, err.Error())
			}
		})
	}
}

// The rule the whole two-projects question turns on: which mounts write, and
// which attach to one pod at a time. They are not the same question — a
// volume read by many is read-only *and* uncapped.
func TestWhatAMountMayDoAndHowManyMayHaveIt(t *testing.T) {
	for mode, want := range map[corev1.PersistentVolumeAccessMode]struct{ writes, once bool }{
		corev1.ReadOnlyMany:  {writes: false, once: false},
		corev1.ReadWriteOnce: {writes: true, once: true},
		corev1.ReadWriteMany: {writes: true, once: false},
	} {
		if Writes(mode) != want.writes || AttachesOnce(mode) != want.once {
			t.Errorf("%s writes=%v attachesOnce=%v, want %+v", mode, Writes(mode), AttachesOnce(mode), want)
		}
	}
}

// Two PersistentVolumes over one export are two objects and one filesystem,
// and the platform has to know it: a name comparison would call them
// unrelated and let two projects write the same twelve terabytes.
func TestTheIdentityIsWhatTheVolumePointsAt(t *testing.T) {
	nfs := func(server, path string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-" + path},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: server, Path: path},
			}},
		}
	}
	first, second := nfs("nas.lan", "/export/media"), nfs("nas.lan", "/export/media/")
	second.Name = "another-object-entirely"
	if VolumeIdentity(first) != "nfs://nas.lan/export/media" {
		t.Errorf("an export is its server and its path: %q", VolumeIdentity(first))
	}
	if VolumeIdentity(first) != VolumeIdentity(second) {
		t.Errorf("two volumes over one export are one identity: %q and %q",
			VolumeIdentity(first), VolumeIdentity(second))
	}
	if VolumeIdentity(nfs("nas.lan", "/export/other")) == VolumeIdentity(first) {
		t.Error("two exports are two identities")
	}

	csi := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-csi"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: "ebs.csi.aws.com", VolumeHandle: "vol-1"},
		}},
	}
	if VolumeIdentity(csi) != "csi://ebs.csi.aws.com/vol-1" {
		t.Errorf("a CSI volume is its driver and its handle: %q", VolumeIdentity(csi))
	}

	// A volume whose source the platform cannot see inside is its own
	// identity: two of them are two, which is the conservative answer.
	opaque := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "mystery"}}
	if VolumeIdentity(opaque) != "persistentVolume://mystery" {
		t.Errorf("an unrecognised source falls back to the object: %q", VolumeIdentity(opaque))
	}
	if VolumeIdentity(nil) != "" {
		t.Error("no volume is no identity")
	}
}

// The bound provider's declaration is the one that says shared, and it may
// only say it because what it shares cannot be written.
func TestTheBoundDeclarationSharesWhatItCannotWrite(t *testing.T) {
	if BoundDeclaration.Preview != contract.PreviewShared || !BoundDeclaration.SharedIsReadOnly {
		t.Errorf("a preview mounts production's own volume, read-only: %+v", BoundDeclaration)
	}
	if BoundDeclaration.PreviewNote == "" || BoundDeclaration.IdleNote == "" || BoundDeclaration.WorkloadNote == "" {
		t.Error("every declaration says why, before any claim exists")
	}
}

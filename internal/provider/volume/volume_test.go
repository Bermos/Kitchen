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

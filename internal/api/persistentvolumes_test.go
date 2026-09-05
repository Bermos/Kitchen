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
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The one step of a bound volume that used to be `kubectl apply -f
// media-pv.yaml` (#406). The two shapes a home installation has, the
// refusals that keep them from becoming something else, and the promise the
// whole surface rests on: nothing here can destroy data.

const volumesPath = "/api/v1/persistent-volumes"

// The NFS export both these tests and the bound-claim tests next door name.
// It is one export in three spellings — the server, the path, and the volume
// over it — and they are constants because the volume tests and the claim
// tests have to be talking about the same storage for either to mean
// anything.
const (
	nasVolume = "nas-media"
	nasServer = "nas.lan"
	nasExport = "/export/media"
)

func getVolume(t *testing.T, h *harness, name string) *corev1.PersistentVolume {
	t.Helper()
	pv := &corev1.PersistentVolume{}
	if err := h.server.Client.Get(t.Context(), types.NamespacedName{Name: name}, pv); err != nil {
		t.Fatal(err)
	}
	return pv
}

// The NFS case: the overwhelming one in the spike's sample, and the one the
// Sonarr and Plex translations both need.
func TestWritingAnNFSVolume(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, volumesPath,
		`{"name": "nas-media", "capacity": "12Ti", "accessModes": ["ReadWriteMany", "ReadOnlyMany"],
			"nfs": {"server": "nas.lan", "path": "/export/media"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[persistentVolumeView](t, recorder)
	if created.Type != "nfs" || created.NFS == nil || created.NFS.Server != nasServer ||
		created.NFS.Path != nasExport {
		t.Errorf("the answer says what the volume points at: %+v", created)
	}
	if created.Identity != "nfs://nas.lan/export/media" {
		t.Errorf("the identity is the one the claim reconciler compares on: %q", created.Identity)
	}
	if created.ReclaimPolicy != string(corev1.PersistentVolumeReclaimRetain) {
		t.Errorf("the reclaim policy is stated rather than implied: %q", created.ReclaimPolicy)
	}

	pv := getVolume(t, h, nasVolume)
	if pv.Labels[managedByLabelKey] != managedByLabelValue {
		t.Errorf("the platform owns what it wrote, and says so on the object: %v", pv.Labels)
	}
	if pv.Annotations[requestedByAnnotation] != testCaller {
		t.Errorf("who asked for it is on the object: %v", pv.Annotations)
	}
	if pv.Spec.NFS == nil || pv.Spec.NFS.Server != nasServer || pv.Spec.NFS.Path != nasExport {
		t.Errorf("the export is written as the mount reads it: %+v", pv.Spec.PersistentVolumeSource)
	}
	if pv.Spec.StorageClassName != "" {
		t.Errorf("a statically written volume belongs to no class, or the claim cut for it never binds: %q",
			pv.Spec.StorageClassName)
	}
	if got := pv.Spec.Capacity[corev1.ResourceStorage]; got.String() != "12Ti" {
		t.Errorf("the capacity is what was asked for: %v", got)
	}
	if len(pv.Spec.AccessModes) != 2 {
		t.Errorf("both modes are written: %v", pv.Spec.AccessModes)
	}
}

// The CSI case: the shape a storage appliance's own driver hands out.
func TestWritingACSIVolume(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, volumesPath,
		`{"name": "appliance-photos", "capacity": "2Ti", "accessModes": ["ReadWriteOnce"],
			"csi": {"driver": "csi.truenas.net", "volumeHandle": "pool/photos", "fsType": "ext4",
			        "volumeAttributes": {"share": "photos"}}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[persistentVolumeView](t, recorder)
	if created.Type != "csi" || created.CSI == nil || created.CSI.Driver != "csi.truenas.net" ||
		created.CSI.VolumeHandle != "pool/photos" {
		t.Errorf("the answer says which driver and which handle: %+v", created)
	}
	if created.Identity != "csi://csi.truenas.net/pool/photos" {
		t.Errorf("the identity is the driver and the handle: %q", created.Identity)
	}

	pv := getVolume(t, h, "appliance-photos")
	if pv.Spec.CSI == nil || pv.Spec.CSI.FSType != "ext4" || pv.Spec.CSI.VolumeAttributes["share"] != "photos" {
		t.Errorf("the driver's own configuration is carried through: %+v", pv.Spec.CSI)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("Retain, always: %q", pv.Spec.PersistentVolumeReclaimPolicy)
	}
}

// `Retain` is not a default that a request can talk out of. Asking for
// anything else is refused rather than quietly overridden, because a caller
// who wrote `Delete` believed it and would go on believing it.
func TestTheReclaimPolicyIsRetainAndCannotBeSetOtherwise(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, volumesPath,
		`{"name": "nas-media", "capacity": "12Ti", "accessModes": ["ReadWriteMany"],
			"persistentVolumeReclaimPolicy": "Delete",
			"nfs": {"server": "nas.lan", "path": "/export/media"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "always Retain") {
		t.Fatalf("the refusal does not say the policy is fixed: %q", got)
	}

	// And asking for it explicitly is the value it already has, so it is
	// accepted rather than refused for being redundant.
	recorder = h.do(t, http.MethodPost, volumesPath,
		`{"name": "nas-media", "capacity": "12Ti", "accessModes": ["ReadWriteMany"],
			"persistentVolumeReclaimPolicy": "Retain",
			"nfs": {"server": "nas.lan", "path": "/export/media"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// hostPath is the spike's boundary, and the refusal has to carry the reason:
// a bare 400 is a refusal somebody works around.
func TestHostPathIsRefusedInWords(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, volumesPath,
		`{"name": "media", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
			"hostPath": {"path": "/srv/media"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	got := errorOf(t, recorder.Body.String())
	for _, want := range []string{"hostPath is refused", "cannot move", "abstracted away", "nfs"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not say %q: %q", want, got)
		}
	}
}

// Nothing credential-shaped reaches the object. A PersistentVolume is
// cluster-scoped and every reader of the cluster can read all of it, so a
// secret reference and a password pasted into a driver attribute are both
// refused at the door.
func TestNoCredentialReachesAVolume(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"a node publish secret": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"csi": {"driver": "csi.example.com", "volumeHandle": "h",
				        "nodePublishSecretRef": {"name": "smb-creds", "namespace": "default"}}}`,
			"csi.nodePublishSecretRef is refused",
		},
		"a node stage secret": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"csi": {"driver": "csi.example.com", "volumeHandle": "h",
				        "nodeStageSecretRef": {"name": "smb-creds"}}}`,
			"csi.nodeStageSecretRef is refused",
		},
		"a password in the driver's attributes": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"csi": {"driver": "csi.example.com", "volumeHandle": "h",
				        "volumeAttributes": {"password": "hunter2"}}}`,
			"reads as a credential",
		},
		"an api key wearing an underscore": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"csi": {"driver": "csi.example.com", "volumeHandle": "h",
				        "volumeAttributes": {"api_key": "abcd"}}}`,
			"reads as a credential",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, volumesPath, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
			pv := &corev1.PersistentVolume{}
			if err := h.server.Client.Get(t.Context(), types.NamespacedName{Name: "v"}, pv); err == nil {
				t.Fatalf("a refused request wrote a volume anyway: %+v", pv.Spec)
			}
		})
	}
}

// Everything else the door refuses, each with the sentence that says what to
// do instead.
func TestTheShapeOfAVolumeRequestIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"no name": {
			`{"capacity": "1Ti", "accessModes": ["ReadWriteOnce"], "nfs": {"server": "n", "path": "/e"}}`,
			"name is required",
		},
		"a name that is not a DNS name": {
			`{"name": "NAS Media", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"nfs": {"server": "n", "path": "/e"}}`,
			"must work as a DNS name",
		},
		"nothing to point at": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"]}`,
			"an nfs block (server, path) or a csi block",
		},
		"both sources": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"nfs": {"server": "n", "path": "/e"}, "csi": {"driver": "d", "volumeHandle": "h"}}`,
			"not both",
		},
		"no server": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"], "nfs": {"path": "/e"}}`,
			"nfs.server is required",
		},
		"a relative export": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
				"nfs": {"server": "n", "path": "export/media"}}`,
			"nfs.path is the exported path",
		},
		"no driver": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"], "csi": {"volumeHandle": "h"}}`,
			"csi.driver is required",
		},
		"no handle": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"], "csi": {"driver": "d"}}`,
			"csi.volumeHandle is required",
		},
		"no capacity": {
			`{"name": "v", "accessModes": ["ReadWriteOnce"], "nfs": {"server": "n", "path": "/e"}}`,
			"capacity is required",
		},
		"a capacity that is not a quantity": {
			`{"name": "v", "capacity": "lots", "accessModes": ["ReadWriteOnce"],
				"nfs": {"server": "n", "path": "/e"}}`,
			"capacity is a Kubernetes quantity",
		},
		"no access modes": {
			`{"name": "v", "capacity": "1Ti", "nfs": {"server": "n", "path": "/e"}}`,
			"accessModes is required",
		},
		"a mode no claim can ask for": {
			`{"name": "v", "capacity": "1Ti", "accessModes": ["ReadWriteOncePod"],
				"nfs": {"server": "n", "path": "/e"}}`,
			"which is not one of",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, volumesPath, testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
		})
	}
}

// A name already taken is a conflict rather than an overwrite: repointing a
// volume something is mounting is how a project silently starts reading
// somebody else's data.
func TestAVolumeNameIsNotOverwritten(t *testing.T) {
	existing := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: nasVolume}}
	h := newHarness(t, nil, append(fixtures(), existing)...)

	recorder := h.do(t, http.MethodPost, volumesPath,
		`{"name": "nas-media", "capacity": "1Ti", "accessModes": ["ReadWriteOnce"],
			"nfs": {"server": "n", "path": "/e"}}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The listing is the platform's own record — what it wrote, and nothing
// else. A volume somebody applied by hand is on `GET /claim-volumes` with
// everything else a claim could bind; it is not something this route claims
// to be accountable for.
func TestTheListingIsWhatThePlatformWrote(t *testing.T) {
	ours := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   nasVolume,
			Labels: map[string]string{managedByLabelKey: managedByLabelValue},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("12Ti")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: nasServer, Path: nasExport},
			},
		},
	}
	theirs := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "hand-written"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: "other.lan", Path: "/export/other"},
			},
		},
	}
	holder := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "sonarr-media", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.ClaimTypeVolume,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Volume: &kitchenv1alpha1.ClaimVolumeStatus{
				Source: kitchenv1alpha1.VolumeBind,
				Bound: &kitchenv1alpha1.ClaimBoundVolume{
					PersistentVolume: nasVolume,
					Identity:         "nfs://nas.lan/export/media",
					Writable:         true,
				},
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), ours, theirs, holder)...)

	recorder := h.do(t, http.MethodGet, volumesPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[persistentVolumesBody](t, recorder)
	if len(body.Items) != 1 || body.Items[0].Name != nasVolume {
		t.Fatalf("the listing is the platform's own volumes alone: %+v", body.Items)
	}
	if got := body.Items[0].HeldBy; len(got) != 1 || got[0] != "shop/sonarr-media" {
		t.Errorf("what mounts the volume is named as project/claim: %v", got)
	}
	if body.Items[0].NFS == nil || body.Items[0].NFS.Server != nasServer {
		t.Errorf("the listing says what the volume points at: %+v", body.Items[0])
	}
}

// Deleting the record deletes the object and nothing else — and is refused
// while anything mounts it, in words naming the claim.
func TestDeletingAVolume(t *testing.T) {
	newVolume := func(name string, labels map[string]string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					NFS: &corev1.NFSVolumeSource{Server: nasServer, Path: "/export/" + name},
				},
			},
		}
	}
	ours := map[string]string{managedByLabelKey: managedByLabelValue}

	t.Run("a volume nothing mounts goes", func(t *testing.T) {
		h := newHarness(t, nil, append(fixtures(), newVolume("spare", ours))...)
		recorder := h.do(t, http.MethodDelete, volumesPath+"/spare", "")
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
		}
		pv := &corev1.PersistentVolume{}
		if err := h.server.Client.Get(t.Context(), types.NamespacedName{Name: "spare"}, pv); err == nil {
			t.Fatal("the volume is still there")
		}
	})

	t.Run("a volume a claim mounts is refused, naming the claim", func(t *testing.T) {
		holder := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "sonarr-media", Namespace: testNamespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
				Type:       kitchenv1alpha1.ClaimTypeVolume,
			},
			Status: kitchenv1alpha1.ResourceClaimStatus{
				Volume: &kitchenv1alpha1.ClaimVolumeStatus{
					Source: kitchenv1alpha1.VolumeBind,
					Bound:  &kitchenv1alpha1.ClaimBoundVolume{PersistentVolume: "media"},
				},
			},
		}
		h := newHarness(t, nil, append(fixtures(), newVolume("media", ours), holder)...)
		recorder := h.do(t, http.MethodDelete, volumesPath+"/media", "")
		if recorder.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "shop/sonarr-media") {
			t.Fatalf("the refusal does not name what mounts it: %q", got)
		}
		getVolume(t, h, "media")
	})

	t.Run("a volume bound outside the platform is refused too", func(t *testing.T) {
		pv := newVolume("elsewhere", ours)
		pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other", Name: "someones-claim"}
		pv.Status.Phase = corev1.VolumeBound
		h := newHarness(t, nil, append(fixtures(), pv)...)
		recorder := h.do(t, http.MethodDelete, volumesPath+"/elsewhere", "")
		if recorder.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "other/someones-claim") {
			t.Fatalf("the refusal does not name the claim holding it: %q", got)
		}
	})

	t.Run("a volume the platform did not write is not the platform's to remove", func(t *testing.T) {
		h := newHarness(t, nil, append(fixtures(), newVolume("hand-written", nil))...)
		recorder := h.do(t, http.MethodDelete, volumesPath+"/hand-written", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
		}
		getVolume(t, h, "hand-written")
	})
}

// The picker's ordering: the volumes the platform wrote come first and say
// so, because they are the ones it can vouch for. The ordering is the API's
// so that every caller gets it, not only the form.
func TestThePlatformsOwnVolumesComeFirstInThePicker(t *testing.T) {
	source := func(export string) corev1.PersistentVolumeSource {
		return corev1.PersistentVolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: nasServer, Path: export},
		}
	}
	handWritten := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "a-hand-written"},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: source("/export/other"),
		},
	}
	written := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "z-platform-wrote-this",
			Labels: map[string]string{managedByLabelKey: managedByLabelValue},
		},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: source(nasExport),
		},
	}
	h := newHarness(t, nil, append(fixtures(), handWritten, written)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-volumes", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[bindableVolumesBody](t, recorder)
	if len(body.PersistentVolumes) != 2 {
		t.Fatalf("both are offered — the list hides no storage: %+v", body.PersistentVolumes)
	}
	if body.PersistentVolumes[0].Name != "z-platform-wrote-this" {
		t.Fatalf("the platform's own come first, even last in the alphabet: %+v", body.PersistentVolumes)
	}
	if !body.PersistentVolumes[0].ManagedByKitchen {
		t.Error("the volume the platform wrote is not marked as one")
	}
	if body.PersistentVolumes[1].ManagedByKitchen {
		t.Error("a volume somebody applied by hand is marked as the platform's")
	}
}

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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// The volume claim through the API: no connection, a required block naming
// the process, and a refusal for anything the reconciler would only refuse
// later.

func TestAVolumeClaimNamesTheProcessThatMountsIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-data", "project": "shop", "type": "volume", "deletionPolicy": "Delete",
			"volume": {"process": "web", "size": "5Gi", "mountPath": "/data", "storageClass": "fast"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[claimView](t, recorder)
	if created.Connection != "" || created.Secret != "" {
		t.Errorf("a volume claim has no connection and no binding secret: %+v", created)
	}
	if created.Volume == nil || created.Volume.Process != "web" || created.Volume.MountPath != "/data" ||
		created.Volume.Size != "5Gi" || created.Volume.StorageClass != "fast" {
		t.Errorf("the answer carries what was asked: %+v", created.Volume)
	}
	if created.DeletionPolicy != "Delete" {
		t.Errorf("a volume holds data, so deletionPolicy is its to choose: %q", created.DeletionPolicy)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-data", claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Volume(); got.Process != "web" || got.MountPath != "/data" || got.Size != "5Gi" {
		t.Errorf("spec.config carries the volume block as the reconciler reads it: %+v", got)
	}
}

func TestTheShapeOfAVolumeRequestIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"no block": {
			`{"name": "v", "project": "shop", "type": "volume"}`,
			"volume is required",
		},
		"no process": {
			`{"name": "v", "project": "shop", "type": "volume", "volume": {"size": "1Gi", "mountPath": "/data"}}`,
			"volume.process is required",
		},
		"a process the project does not have": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "worker", "size": "1Gi", "mountPath": "/data"}}`,
			"not one of project shop's processes",
		},
		"a relative mount path": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "data"}}`,
			"volume.mountPath must be an absolute",
		},
		"a size that is not a quantity": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "web", "size": "lots", "mountPath": "/data"}}`,
			"volume.size is a Kubernetes quantity",
		},
		"a connection": {
			`{"name": "v", "project": "shop", "type": "volume", "connection": "postgres",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`,
			"takes no connection",
		},
		"sharing production's volume with previews": {
			`{"name": "v", "project": "shop", "type": "volume", "previewMode": "shared",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`,
			"previewMode \"shared\" is refused",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
		})
	}
}

// The catalogue says, before the claim exists, that a provisioned volume
// forces a recreate and offers previews a fresh volume or nothing — never
// production's own — and that a bound one gives previews production's own
// volume read-only, which is the only sharing that takes nothing away.
func TestTheCatalogueDeclaresWhatAVolumeCosts(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-types", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	for _, view := range decode[[]claimTypeView](t, recorder) {
		if view.Type != kitchenv1alpha1.ClaimTypeVolume {
			continue
		}
		if view.Capability != "" || !view.HoldsData || len(view.Providers) != 2 {
			t.Fatalf("a volume takes no connection, holds data and is cut or bound: %+v", view)
		}
		provider := view.Providers[0]
		if provider.Provider != volume.ProviderName || provider.PreviewMode != string(contract.PreviewFresh) {
			t.Errorf("the StorageClass gives previews a fresh volume: %+v", provider)
		}
		if !provider.ForcesRecreate || provider.WorkloadNote == "" {
			t.Errorf("a volume declares the downtime it costs: %+v", provider)
		}
		for _, choice := range provider.PreviewChoices {
			if choice == string(contract.PreviewShared) {
				t.Errorf("a provisioned volume cannot be shared with previews, and must not be offered: %v",
					provider.PreviewChoices)
			}
		}
		bound := view.Providers[1]
		if bound.Provider != volume.BoundProviderName || bound.PreviewMode != string(contract.PreviewShared) ||
			!bound.SharedIsReadOnly {
			t.Errorf("a bound volume gives previews the same volume read-only: %+v", bound)
		}
		return
	}
	t.Fatal("the catalogue does not list the volume type")
}

// The other source: a volume the platform did not create. Everything the
// door can decide from the request alone is decided here, because a claim
// that is about to fail is worse than a request that was refused.

func TestAVolumeClaimCanBindOneThatAlreadyExists(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "media", "project": "shop", "type": "volume",
			"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			           "bind": {"persistentVolume": "nas-media", "accessMode": "ReadOnlyMany"}}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[claimView](t, recorder)
	if created.Volume == nil || created.Volume.Source != string(kitchenv1alpha1.VolumeBind) {
		t.Fatalf("the answer says which of the two shapes it is: %+v", created.Volume)
	}
	if created.Volume.Size != "" || created.Volume.StorageClass != "" {
		t.Errorf("nothing is being cut, so there is no size and no class: %+v", created.Volume)
	}
	if created.Volume.Bind == nil || created.Volume.Bind.PersistentVolume != nasVolume ||
		created.Volume.Bind.AccessMode != "ReadOnlyMany" {
		t.Errorf("the answer carries what was named: %+v", created.Volume.Bind)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "media", claim); err != nil {
		t.Fatal(err)
	}
	got := claim.Volume()
	if !got.Bound() || got.Bind == nil || got.Bind.PersistentVolume != nasVolume {
		t.Errorf("spec.config carries the binding as the reconciler reads it: %+v", got)
	}
}

// A claim written before binding existed said nothing about a source, and
// means the one thing it could have meant.
func TestAVolumeClaimThatNamesNoSourceProvisions(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "d", "project": "shop", "type": "volume",
			"volume": {"process": "web", "size": "1Gi", "mountPath": "/data"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if created := decode[claimView](t, recorder); created.Volume.Source != string(kitchenv1alpha1.VolumeProvision) {
		t.Errorf("source = %q, want the answer to say so rather than leave it to be inferred", created.Volume.Source)
	}
}

func TestTheShapeOfABoundVolumeIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, testCase := range map[string]struct {
		body string
		says string
	}{
		"a size on a volume nothing is cutting": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m", "size": "1Gi",
				           "bind": {"persistentVolume": "nas", "accessMode": "ReadOnlyMany"}}}`,
			"volume.size is refused on a bind volume",
		},
		"a class on a volume nothing is cutting": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m", "storageClass": "fast",
				           "bind": {"persistentVolume": "nas", "accessMode": "ReadOnlyMany"}}}`,
			"volume.storageClass is refused on a bind volume",
		},
		"a binding on a volume being cut": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"process": "web", "size": "1Gi", "mountPath": "/m",
				           "bind": {"persistentVolume": "nas", "accessMode": "ReadOnlyMany"}}}`,
			"volume.bind is refused on a provision volume",
		},
		"naming no volume at all": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m",
				           "bind": {"accessMode": "ReadOnlyMany"}}}`,
			"volume.bind names no volume",
		},
		"naming two": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m",
				           "bind": {"persistentVolume": "nas", "persistentVolumeClaim": "old",
				                    "accessMode": "ReadOnlyMany"}}}`,
			"names both",
		},
		"no access mode": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m",
				           "bind": {"persistentVolume": "nas"}}}`,
			"volume.bind.accessMode is required",
		},
		"a source that is neither": {
			`{"name": "v", "project": "shop", "type": "volume",
				"volume": {"source": "borrow", "process": "web", "mountPath": "/m"}}`,
			"volume.source must be",
		},
		// The acceptance criterion this one is: a policy that would destroy
		// a volume the platform did not create never reaches an object.
		"a policy that would destroy somebody else's data": {
			`{"name": "v", "project": "shop", "type": "volume", "deletionPolicy": "Delete",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m",
				           "bind": {"persistentVolume": "nas", "accessMode": "ReadOnlyMany"}}}`,
			"deletionPolicy Delete is refused on a bound volume",
		},
		// A volume that attaches to one pod at a time is production's while
		// production runs, so sharing it with a preview would take it away.
		"sharing a single-attach volume with previews": {
			`{"name": "v", "project": "shop", "type": "volume", "previewMode": "shared",
				"volume": {"source": "bind", "process": "web", "mountPath": "/m",
				           "bind": {"persistentVolume": "nas", "accessMode": "ReadWriteOnce"}}}`,
			"is refused for a volume mounted ReadWriteOnce",
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims", testCase.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say %q: %q", testCase.says, got)
			}
		})
	}
}

// A preview mounting a volume read-only takes nothing from production and
// changes nothing on it, which is why this is the one provider that may be
// asked to share what it holds however it deploys.
func TestPreviewsMaySharePlainlyReadableStorage(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "media", "project": "shop", "type": "volume", "previewMode": "shared",
			"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			           "bind": {"persistentVolume": "nas-media", "accessMode": "ReadOnlyMany"}}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// What a claim could bind, answered before the claim is written. A volume
// nothing can mount is still listed — a name somebody was told to use must
// not simply fail to appear — and a volume somebody already writes says so
// rather than being offered and refused a moment later.
func TestListingWhatAVolumeClaimCouldBind(t *testing.T) {
	nas := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: nasVolume},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("12Ti")},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: nasServer, Path: nasExport},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
	// A second volume over the same export, which is how two projects reach
	// one filesystem at all — and the reason the comparison is on what the
	// volumes point at rather than on their names.
	sameExport := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "nas-media-2"},
		Spec: corev1.PersistentVolumeSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{Server: nasServer, Path: nasExport},
			},
		},
	}
	writer := &kitchenv1alpha1.ResourceClaim{
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
	h := newHarness(t, nil, append(fixtures(), nas, sameExport, writer)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-volumes", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[bindableVolumesBody](t, recorder)
	if len(body.PersistentVolumes) != 2 {
		t.Fatalf("both volumes are listed, whether or not either can be bound: %+v", body.PersistentVolumes)
	}
	for _, view := range body.PersistentVolumes {
		if view.Identity != "nfs://nas.lan/export/media" {
			t.Errorf("%s: two volumes over one export are one identity: %q", view.Name, view.Identity)
		}
		if view.Writable {
			t.Errorf("%s: something already writes this storage, and one filesystem has one writer", view.Name)
		}
		if !view.Readable {
			t.Errorf("%s: a volume that may be written by many may be read by many", view.Name)
		}
		if !strings.Contains(view.Note, "one filesystem has one writer") {
			t.Errorf("%s: the reason a mount is refused is not said: %q", view.Name, view.Note)
		}
	}
	if got := body.PersistentVolumes[0].HeldBy; len(got) != 1 || got[0] != "shop/sonarr-media" {
		t.Errorf("the claim already holding the storage is named: %v", got)
	}
}

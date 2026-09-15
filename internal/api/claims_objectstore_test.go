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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

func s3Connection() *kitchenv1alpha1.Connection {
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "store", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             objectstore.ProviderS3,
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "kitchen-connection-store"},
		},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityObjectStore},
		},
	}
}

func TestAnObjectStoreClaimAsksForABucket(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), s3Connection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "store", "type": "objectStore",
		  "objectStore": {"versioning": true, "size": "50Gi"}, "previewMode": "fresh"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var view claimView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ObjectStore == nil || !view.ObjectStore.Versioning || view.ObjectStore.Size != "50Gi" ||
		view.ObjectStore.PublicRead {
		t.Errorf("the answer carries what was asked: %+v", view.ObjectStore)
	}
	if view.PreviewChoice != "fresh" || view.DeletionPolicy != "" {
		t.Errorf("preview choice and the default policy: %+v", view)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-uploads", claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.ObjectStore(); !got.Versioning || got.Size != "50Gi" {
		t.Errorf("the reconciler reads the same block back: %+v", got)
	}
	if claim.Spec.ConnectionRef == nil || claim.Spec.ConnectionRef.Name != "store" {
		t.Error("the claim names the store's connection")
	}
}

func TestAnObjectStoreClaimThatAsksForNothingCarriesNoBlock(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), s3Connection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "store", "type": "objectStore",
		  "objectStore": {}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var view claimView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ObjectStore != nil {
		t.Errorf("an empty block is nothing rather than an empty object: %+v", view.ObjectStore)
	}
}

func TestAnObjectStoreClaimIsRefusedABadSizeAndAWrongConnection(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), s3Connection(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "store", "type": "objectStore",
		  "objectStore": {"size": "lots"}}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, recorder.Body.String()), "objectStore.size") {
		t.Errorf("a size that is not a quantity is refused by name: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "postgres", "type": "objectStore"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, recorder.Body.String()), "objectStore capability") {
		t.Errorf("a database connection cannot provision a bucket: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-uploads", "project": "shop", "connection": "store", "type": "objectStore",
		  "previewMode": "branch"}`)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, recorder.Body.String()), "fresh") {
		t.Errorf("s3 gives previews a fresh bucket, and a claim asking for a branch hears so: %d %s",
			recorder.Code, recorder.Body.String())
	}
}

func TestClaimTypesListTheObjectStore(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claim-types", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", recorder.Code)
	}
	var types []claimTypeView
	if err := json.Unmarshal(recorder.Body.Bytes(), &types); err != nil {
		t.Fatal(err)
	}
	for _, claimType := range types {
		if claimType.Type != kitchenv1alpha1.ClaimTypeObjectStore {
			continue
		}
		if claimType.Capability != string(kitchenv1alpha1.CapabilityObjectStore) || !claimType.HoldsData {
			t.Errorf("objectStore takes an objectStore connection and holds data: %+v", claimType)
		}
		if len(claimType.Providers) != 1 || claimType.Providers[0].Provider != objectstore.ProviderS3 ||
			claimType.Providers[0].PreviewMode != "fresh" {
			t.Errorf("s3 is the one provider and it gives previews a fresh bucket: %+v", claimType.Providers)
		}
		return
	}
	t.Error("objectStore is not among the claim types")
}

// The two addresses, where a developer reads about the claim. They are not
// credentials — the store admits nobody anonymously — and the difficulty the
// claim type now has is knowing which of them to presign against, so the
// answer is on the claim rather than only in a Secret nothing reads back
// (#601).
func boundObjectStoreClaim(where *kitchenv1alpha1.ClaimObjectStoreStatus) *kitchenv1alpha1.ResourceClaim {
	return &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-uploads", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "store"},
			Type:          kitchenv1alpha1.ClaimTypeObjectStore,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Phase:       kitchenv1alpha1.ClaimBound,
			SecretName:  "shop-uploads-binding",
			ObjectStore: where,
		},
	}
}

// A binding with no publicEndpoint has two opposite meanings and the claim
// has to carry which one, or the screen reading it tells half its readers to
// presign against an address no browser resolves — the bug #601 was filed
// from, rendered as advice (#611 review).
func TestAnAbsentPublicEndpointSaysWhichKindOfAbsenceItIs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		where     *kitchenv1alpha1.ClaimObjectStoreStatus
		inCluster bool
	}{
		{
			// The bundled store with nothing published in front of it:
			// tls.mode none, no base domain, or a Gateway API without
			// BackendTLSPolicy. Nothing presigned here opens in a browser.
			name: "the bundled store, published nowhere",
			where: &kitchenv1alpha1.ClaimObjectStoreStatus{
				Endpoint:  "https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000",
				InCluster: true,
			},
			inCluster: true,
		},
		{
			// A store of somebody else's. Its one address is already
			// public, and presigning against it is exactly right.
			name: "a store of somebody else's",
			where: &kitchenv1alpha1.ClaimObjectStoreStatus{
				Endpoint: "https://s3.eu-central-1.amazonaws.com",
			},
			inCluster: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil, append(fixtures(), s3Connection(), boundObjectStoreClaim(tc.where))...)

			recorder := h.do(t, http.MethodGet, "/api/v1/claims/shop-uploads", "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
			}
			var view claimView
			if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.ObjectStore == nil {
				t.Fatal("a bound bucket says where it is")
			}
			if view.ObjectStore.PublicEndpoint != "" {
				t.Fatalf("this case is about the absence: %q", view.ObjectStore.PublicEndpoint)
			}
			if view.ObjectStore.InCluster != tc.inCluster {
				t.Errorf("inCluster is %v; it is the whole of what the absence means",
					view.ObjectStore.InCluster)
			}
		})
	}
}

func TestABoundObjectStoreClaimAnswersWithBothAddresses(t *testing.T) {
	bound := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-uploads", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "store"},
			Type:          kitchenv1alpha1.ClaimTypeObjectStore,
		},
		Status: kitchenv1alpha1.ResourceClaimStatus{
			Phase:      kitchenv1alpha1.ClaimBound,
			SecretName: "shop-uploads-binding",
			ObjectStore: &kitchenv1alpha1.ClaimObjectStoreStatus{
				Endpoint:       "https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000",
				PublicEndpoint: "https://objectstore.apps.example.com",
				InCluster:      true,
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), s3Connection(), bound)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/claims/shop-uploads", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var view claimView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ObjectStore == nil {
		t.Fatal("a bound bucket says where it is, even when the claim asked for nothing in particular")
	}
	if view.ObjectStore.Endpoint != "https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000" {
		t.Errorf("the in-cluster address is the application's own: %q", view.ObjectStore.Endpoint)
	}
	if view.ObjectStore.PublicEndpoint != "https://objectstore.apps.example.com" {
		t.Errorf("the published address is what a URL for somebody else is signed against: %q",
			view.ObjectStore.PublicEndpoint)
	}
	if strings.Contains(recorder.Body.String(), "secretAccessKey") {
		t.Error("an address is not a credential, and the binding's keys are still never read back")
	}
}

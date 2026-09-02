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

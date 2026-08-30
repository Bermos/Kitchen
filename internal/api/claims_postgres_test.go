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
	"net/http"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// cnpgConnection is the self-hosted database provider as the Connection
// reconciler leaves one: no credentials secret at all, because it provisions
// with the operator's own account.
func cnpgConnection() *kitchenv1alpha1.Connection {
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "postgres", Namespace: testNamespace},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "cnpg"},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityDatabase},
		},
	}
}

func TestAClaimCanAskForAVersionExtensionsAndStorage(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "maps-db", "project": "shop", "connection": "postgres", "type": "postgres",
			"postgres": {"version": "17", "extensions": ["postgis", "vector"],
				"storage": {"size": "40Gi", "storageClass": "fast-ssd"}}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[claimView](t, recorder)
	if view.Postgres == nil {
		t.Fatal("the answer does not carry what was asked for")
	}
	if view.Postgres.Version != "17" || view.Postgres.StorageSize != "40Gi" ||
		view.Postgres.StorageClass != "fast-ssd" ||
		!slices.Equal(view.Postgres.Extensions, []string{"postgis", "vector"}) {
		t.Fatalf("unexpected answer: %+v", view.Postgres)
	}

	stored := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(context.Background(), "maps-db", stored); err != nil {
		t.Fatal(err)
	}
	postgres := stored.Postgres()
	if postgres.Version != "17" || len(postgres.Extensions) != 2 || postgres.Storage.Size != "40Gi" {
		t.Fatalf("the requirements did not reach spec.config: %+v", postgres)
	}
}

// A claim that asks for nothing in particular carries nothing in particular:
// an empty postgres block on the spec would say a developer chose defaults
// they never saw.
func TestAClaimThatAsksForNothingCarriesNoPostgresBlock(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "plain-db", "project": "shop", "connection": "postgres", "type": "postgres",
			"postgres": {"extensions": []}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[claimView](t, recorder); view.Postgres != nil {
		t.Fatalf("nothing was asked for, but the answer carries %+v", view.Postgres)
	}
}

// Shape is this layer's to refuse; availability is the provisioner's, and the
// refusals have to read differently because the fixes are different.
func TestTheShapeOfAPostgresRequestIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	for _, testCase := range []struct{ name, body, says string }{
		{
			name: "a version that is not a major",
			body: `"postgres": {"version": "17.2"}`,
			says: "major version",
		},
		{
			name: "an extension that is not an identifier",
			body: `"postgres": {"extensions": ["vector\"; DROP DATABASE app; --"]}`,
			says: "CREATE EXTENSION takes",
		},
		{
			name: "a size that is not a quantity",
			body: `"postgres": {"storage": {"size": "quite big"}}`,
			says: "Kubernetes quantity",
		},
		{
			name: "a size of nothing",
			body: `"postgres": {"storage": {"size": "0"}}`,
			says: "more than nothing",
		},
		{
			name: "a storage class that is not a name",
			body: `"postgres": {"storage": {"storageClass": "Fast SSD"}}`,
			says: "StorageClass name",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/claims",
				`{"name": "bad-db", "project": "shop", "connection": "postgres", "type": "postgres", `+
					testCase.body+`}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, testCase.says) {
				t.Fatalf("the refusal does not say what is wrong: %q", got)
			}
		})
	}
}

func TestAnOIDCClaimTakesNoPostgresBlock(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), cnpgConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-auth", "project": "shop", "type": "oidcClient", "postgres": {"version": "17"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "no volume") {
		t.Fatalf("the refusal does not say why: %q", got)
	}
}

// The one provider with nothing to store. A credential is not merely
// unnecessary — it is refused, because one somebody rotates believing it
// matters is worse than none at all.
func TestACnpgConnectionIsCreatedWithoutACredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "postgres", "provider": "cnpg"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Connection{}
	if err := h.server.get(context.Background(), "postgres", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.CredentialsSecretRef.Name != "" {
		t.Fatalf("a credentials secret was named for a provider that has none: %q",
			stored.Spec.CredentialsSecretRef.Name)
	}
}

func TestACnpgConnectionRefusesACredential(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections",
		`{"name": "postgres", "provider": "cnpg", "credential": {"token": "hunter2"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "takes no credential") {
		t.Fatalf("the refusal does not say why: %q", got)
	}
}

func TestEveryOtherProviderStillNeedsOne(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections", `{"name": "neon2", "provider": "neon"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "credential is required") {
		t.Fatalf("unexpected refusal: %q", got)
	}
}

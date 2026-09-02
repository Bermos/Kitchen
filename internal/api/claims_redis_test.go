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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
)

// The redis claim through the API: the two fields that decide what the claim
// costs and what it can promise — `usage` and `tenancy` — are checked here
// for shape, and whether a connection can honour them is the provisioner's
// answer on the claim.

func valkeyConnection() *kitchenv1alpha1.Connection {
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "valkey", Namespace: testNamespace},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: cache.ProviderValkey},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityCache},
		},
	}
}

// A claim that names no tenancy asks for nothing: the platform resolves one,
// and writing a default down here would make the claim look as though it had
// insisted on a shape it never chose.
func TestARedisClaimCarriesTheTenancyItAskedForAndNoMore(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), valkeyConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-jobs", "project": "shop", "connection": "valkey", "type": "redis",
			"redis": {"usage": "queue", "tenancy": "dedicated"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	created := decode[claimView](t, recorder)
	if created.Redis == nil || created.Redis.Tenancy != string(cache.TenancyDedicated) {
		t.Fatalf("the answer carries what was asked: %+v", created.Redis)
	}

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := h.server.get(t.Context(), "shop-jobs", claim); err != nil {
		t.Fatal(err)
	}
	if got := claim.Redis(); got.Tenancy != string(cache.TenancyDedicated) || got.Usage != string(cache.UsageQueue) {
		t.Errorf("spec.config carries the block as the reconciler reads it: %+v", got)
	}

	recorder = h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-cache", "project": "shop", "connection": "valkey", "type": "redis"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if plain := decode[claimView](t, recorder); plain.Redis != nil {
		t.Errorf("a claim that asked for nothing must not answer a shape it never chose: %+v", plain.Redis)
	}
}

func TestARedisTenancyThatIsNeitherIsRefusedHere(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), valkeyConnection())...)

	recorder := h.do(t, http.MethodPost, "/api/v1/claims",
		`{"name": "shop-cache", "project": "shop", "connection": "valkey", "type": "redis",
			"redis": {"tenancy": "isolated"}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, says := range []string{string(cache.TenancyShared), string(cache.TenancyDedicated)} {
		if !strings.Contains(body, says) {
			t.Errorf("the refusal must name the two shapes there are (%q): %s", says, body)
		}
	}
}

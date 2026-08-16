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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The domain write surface: attaching a hostname is a create that answers
// with instructions rather than a result — DNS is the user's move — and a
// delete is finished asynchronously by the operator's finalizer.

func TestAttachingADomain(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "Store.Example.NET.", "environment": "shop-production", "tls": "acme"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[domainView](t, recorder)
	if view.Hostname != "store.example.net" {
		t.Fatalf("the hostname should be normalized to lowercase without a trailing dot, got %q", view.Hostname)
	}
	if view.Name != "store-example-net" {
		t.Fatalf("the name should be derived from the hostname, got %q", view.Name)
	}
	if view.Environment != testEnvironment || view.TLS != "acme" {
		t.Fatalf("the response does not echo the request: %+v", view)
	}

	stored := &kitchenv1alpha1.Domain{}
	if err := h.server.get(context.Background(), view.Name, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Hostname != "store.example.net" ||
		stored.Spec.EnvironmentRef.Name != testEnvironment ||
		stored.Spec.TLS != kitchenv1alpha1.TLSModeACME {
		t.Fatalf("the spec did not stick: %+v", stored.Spec)
	}
	if stored.Annotations[requestedByAnnotation] == "" {
		t.Fatal("a domain should record who attached it")
	}
}

func TestAttachingADomainRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"no hostname":                  `{"environment": "shop-production"}`,
		"a bare label":                 `{"hostname": "shop", "environment": "shop-production"}`,
		"not a DNS name":               `{"hostname": "sh op.example.com", "environment": "shop-production"}`,
		"a name under the base domain": `{"hostname": "shop.apps.example.com", "environment": "shop-production"}`,
		"a tls mode that is not one":   `{"hostname": "store.example.net", "environment": "shop-production", "tls": "edge"}`,
		"no environment":               `{"hostname": "store.example.net"}`,
		"an environment that is not there": `{"hostname": "store.example.net",
			"environment": "nope-production"}`,
		"a name that is not a label": `{"hostname": "store.example.net",
			"environment": "shop-production", "name": "Store.Net"}`,
		"an unknown field": `{"host": "store.example.net"}`,
		"not JSON":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/domains", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestAttachingAHostnameTwice(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	// The fixture domain already claims shop.example.com.
	recorder := h.do(t, http.MethodPost, "/api/v1/domains",
		`{"hostname": "shop.example.com", "environment": "shop-production"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestDomainViewCarriesTheVerificationInstructions(t *testing.T) {
	domain := &kitchenv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "store-example-net", Namespace: testNamespace},
		Spec: kitchenv1alpha1.DomainSpec{
			Hostname:       "store.example.net",
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: testEnvironment},
		},
		Status: kitchenv1alpha1.DomainStatus{
			Verified: false,
			TLSMode:  kitchenv1alpha1.TLSModeACME,
			Verification: &kitchenv1alpha1.DomainVerification{
				TXTRecord:   "_kitchen-challenge.store.example.net",
				TXTValue:    "kitchen-verify=0123",
				CNAMETarget: "shop.apps.example.com",
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), domain)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/domains/store-example-net", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[domainView](t, recorder)
	if view.Verification == nil {
		t.Fatal("the view should carry the record the user has to create")
	}
	if view.Verification.TXTRecord != "_kitchen-challenge.store.example.net" ||
		view.Verification.TXTValue != "kitchen-verify=0123" ||
		view.Verification.CNAMETarget != "shop.apps.example.com" {
		t.Fatalf("the verification instructions did not carry: %+v", view.Verification)
	}
	if view.EffectiveTLS != "acme" {
		t.Fatalf("the view should say which TLS mode is in effect, got %q", view.EffectiveTLS)
	}
}

func TestDeletingADomain(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/domains/shop-com", "")
	// 202: the operator's finalizer still has the certificate and its secret
	// to remove when this response goes out.
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop-com", &kitchenv1alpha1.Domain{}); err == nil {
		t.Fatal("the domain is still there")
	}
}

func TestDeletingADomainThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodDelete, "/api/v1/domains/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

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
	"net/url"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The hostname a domain is attached under in these fixtures, and the one the
// exclusion is about.
const attachedHost = "app.example.com"

// attached is the environment on the edge with one custom domain on it, in
// whatever state of coming up the case is about. A domain with no
// RouteProgrammed condition at all is a domain the reconciler has not judged
// yet, which is the state it is created in.
//
// The environment's route carries the hostname, because that is what a
// verified domain waiting on the gateway looks like — and it is what makes the
// traffic on the name this environment's to begin with.
func attached(conditions ...metav1.Condition) []runtime.Object {
	return append(onTheEdgeServing(attachedHost), domainOn(testEnvironment, attachedHost, conditions...))
}

// domainOn is a custom domain attached to an environment.
func domainOn(environment, hostname string, conditions ...metav1.Condition) *kitchenv1alpha1.Domain {
	return &kitchenv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "app-example-com", Namespace: testNamespace},
		Spec: kitchenv1alpha1.DomainSpec{
			Hostname:       hostname,
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: environment},
		},
		Status: kitchenv1alpha1.DomainStatus{Verified: true, Conditions: conditions},
	}
}

// withoutDomains drops every Domain from a fixture set. fixtures() carries one
// — a working one, which is the right default for a set every request test
// uses — so it is what "this environment has no custom domain" has to be built
// from.
func withoutDomains(objects []runtime.Object) []runtime.Object {
	kept := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		if _, isDomain := object.(*kitchenv1alpha1.Domain); isDomain {
			continue
		}
		kept = append(kept, object)
	}
	return kept
}

// routeProgrammed is the condition the domain reconciler writes once the
// gateway has accepted the route on the listener this hostname's traffic uses.
func routeProgrammed(status metav1.ConditionStatus, reason string) metav1.Condition {
	return metav1.Condition{
		Type:               controller.ConditionRouteProgrammed,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.Now(),
	}
}

// The case in the report: a verified domain whose route the gateway has not
// accepted yet. Its hostname is on the environment's route, so the follower
// attributes the traffic to the environment — but the edge is what answers it,
// and the environment's own numbers must not count it.
func TestTrafficOnAHostnameTheEdgeAnswersIsNotTheEnvironments(t *testing.T) {
	h := newHarness(t, nil, attached(routeProgrammed(
		metav1.ConditionFalse, "AwaitingGatewayAcceptance"))...)

	for _, read := range []struct {
		name string
		path string
		// asked is the exclusion the store was handed for this read.
		asked func(*harness) []string
	}{
		{"summary", summaryPath, func(h *harness) []string { return h.logs.lastRequestSummary.ExcludeHosts }},
		{"series", seriesPath, func(h *harness) []string { return h.logs.lastRequestSeries.ExcludeHosts }},
		{"routes", routesPath, func(h *harness) []string { return h.logs.lastRequestRoutes.ExcludeHosts }},
		{"listing", requestsPath, func(h *harness) []string { return h.logs.lastRequests.ExcludeHosts }},
	} {
		t.Run(read.name, func(t *testing.T) {
			res := h.do(t, http.MethodGet, read.path, "")
			if res.Code != http.StatusOK {
				t.Fatalf("GET %s = %d: %s", read.path, res.Code, res.Body.String())
			}
			if got := read.asked(h); !slices.Equal(got, []string{attachedHost}) {
				t.Fatalf("the store should have been asked to drop %q, got %v", attachedHost, got)
			}
		})
	}

	// And every answer says so, because a number that silently dropped rows is
	// a number nobody can reconcile.
	res := h.do(t, http.MethodGet, summaryPath, "")
	body := decode[requestSummaryBody](t, res)
	if !slices.Equal(body.PendingDomains.Hostnames, []string{attachedHost}) || !body.PendingDomains.Excluded {
		t.Errorf("the answer should name what it left out: %+v", body.PendingDomains)
	}
}

// A domain the gateway has accepted is a hostname the environment really is
// serving, and its traffic is the environment's own.
func TestAServedHostnameIsCountedLikeAnyOther(t *testing.T) {
	h := newHarness(t, nil, attached(routeProgrammed(metav1.ConditionTrue, "Accepted"))...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if len(body.PendingDomains.Hostnames) != 0 || body.PendingDomains.Excluded {
		t.Errorf("a routed hostname is not pending: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

// A domain of another environment is another environment's business, however
// stuck it is.
func TestAnotherEnvironmentsPendingDomainIsNotExcludedHere(t *testing.T) {
	h := newHarness(t, nil, append(onTheEdgeServing(attachedHost),
		domainOn("shop-preview-12", attachedHost))...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	if body := decode[requestSummaryBody](t, res); len(body.PendingDomains.Hostnames) != 0 {
		t.Errorf("another environment's domain reached this answer: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

// The older question is still askable, and it is one parameter — which is what
// makes the traffic evidence rather than something the screen hid.
func TestPendingDomainsCanBeCountedBackIn(t *testing.T) {
	h := newHarness(t, nil, attached()...)

	res := h.do(t, http.MethodGet, summaryPath+"?pending=include", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	// The hostname is still named: it is what the screen offers to drop again.
	if !slices.Equal(body.PendingDomains.Hostnames, []string{attachedHost}) || body.PendingDomains.Excluded {
		t.Errorf("nothing should have been excluded: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store was still asked to exclude: %+v", h.logs.lastRequestSummary)
	}
}

// A route filter does not cancel it: the same template is served on every name
// the environment answers to, so naming one says nothing about which of those
// names was asked.
func TestFilteringToARouteStillDropsAPendingDomain(t *testing.T) {
	h := newHarness(t, nil, attached()...)

	res := h.do(t, http.MethodGet, summaryPath+"?route="+url.QueryEscape(checkout), "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	if got := h.logs.lastRequestSummary.ExcludeHosts; !slices.Equal(got, []string{attachedHost}) {
		t.Errorf("the hostname exclusion should survive a route filter, got %v", got)
	}
}

// An environment with no custom domain has nothing to say and asks the store
// for everything. fixtures() carries one, so it is taken back out: a case that
// left it in would be a second copy of the served-hostname case above.
func TestAnEnvironmentWithNoDomainsExcludesNothing(t *testing.T) {
	h := newHarness(t, nil, withoutDomains(onTheEdgeServing("shop.example.com"))...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if len(body.PendingDomains.Hostnames) != 0 || body.PendingDomains.Excluded {
		t.Errorf("nothing is attached, so nothing is pending: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

func TestAnUnknownPendingValueNamesTheChoices(t *testing.T) {
	h := newHarness(t, nil, attached()...)

	res := h.do(t, http.MethodGet, summaryPath+"?pending=maybe", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "include") || !strings.Contains(res.Body.String(), "exclude") {
		t.Errorf("the answer should name the choices: %s", res.Body.String())
	}
}

// An unverified domain is on no route, so nothing was ever filed under this
// environment for its hostname — and a note naming it would be a permanent
// sentence about traffic that cannot exist, on the screen of the person still
// waiting for their DNS record to be seen.
func TestAnUnverifiedDomainIsNotNamedAsExcluded(t *testing.T) {
	unverified := domainOn(testEnvironment, attachedHost,
		routeProgrammed(metav1.ConditionFalse, "AwaitingVerification"))
	unverified.Status.Verified = false
	// The route is the environment's own generated hostname and nothing else,
	// which is what a route looks like before a domain has been verified onto
	// it.
	h := newHarness(t, nil, append(onTheEdgeServing("shop.apps.example.com"), unverified)...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if len(body.PendingDomains.Hostnames) != 0 || body.PendingDomains.Excluded {
		t.Errorf("a hostname on no route has no traffic here to leave out: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

// The same shape for an environment nothing publishes at all — an
// `internal`-exposure project, where the domain reconciler answers
// `RouteMissing` forever. There is no route, so there is no attribution, so
// there is nothing to name.
func TestAnEnvironmentOffTheEdgeNamesNoPendingDomain(t *testing.T) {
	// fixtures() has no HTTPRoute; this environment is not on the edge.
	h := newHarness(t, nil, append(fixtures(),
		domainOn(testEnvironment, attachedHost, routeProgrammed(metav1.ConditionFalse, "RouteMissing")))...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.Edge.Routed {
		t.Fatalf("this environment is meant to be off the edge: %+v", body.Edge)
	}
	if len(body.PendingDomains.Hostnames) != 0 || body.PendingDomains.Excluded {
		t.Errorf("nothing publishes this environment, so nothing is pending: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

// A wildcard is attributed by the follower and cannot be excluded by a
// `host NOT IN (...)` over concrete names, so it is not claimed as excluded
// either. The API refuses to create one; the CRD does not.
func TestAWildcardDomainIsNotClaimedAsExcluded(t *testing.T) {
	const wildcard = "*.example.com"
	h := newHarness(t, nil, append(onTheEdgeServing(wildcard),
		domainOn(testEnvironment, wildcard, routeProgrammed(metav1.ConditionFalse, "AwaitingGatewayAcceptance")))...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if len(body.PendingDomains.Hostnames) != 0 || body.PendingDomains.Excluded {
		t.Errorf("an exclusion that cannot drop a row must not be claimed: %+v", body.PendingDomains)
	}
	if h.logs.lastRequestSummary.ExcludeHosts != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

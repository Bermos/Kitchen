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
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The paths under test, built from one prefix so that a route rename is one
// edit rather than a search.
const (
	requestsPath = "/api/v1/environments/" + testEnvironment + "/requests"
	summaryPath  = requestsPath + "/summary"
	seriesPath   = requestsPath + "/series"
	routesPath   = requestsPath + "/routes"
)

// checkout is the route template these tests filter by — one path, so that the
// filter reaching the store is visibly the one that was asked for.
const checkout = "/checkout/:id"

// onTheEdge is the environment's HTTPRoute, which is what publishes it on the
// shared Gateway. Its presence is the difference between "nothing was asked of
// this environment" and "nothing can reach it".
func onTheEdge() []runtime.Object {
	return append(fixtures(), &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name:      testEnvironment,
		Namespace: controller.AppNamespace(feedProject),
	}})
}

func TestRequestSummary(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	answered := time.Now().Add(-time.Hour).UTC().Truncate(time.Minute)
	h.logs.requestSummary = clickhouse.RequestSummary{
		Since:             answered,
		Until:             answered.Add(time.Hour),
		Requests:          3600,
		RequestsPerSecond: 1,
		Errors:            36,
		ErrorRate:         0.01,
		P95Ms:             240,
		Rollup:            clickhouse.RequestRollupMinute,
	}

	res := h.do(t, http.MethodGet, summaryPath+"?route="+url.QueryEscape(checkout), "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.Requests != 3600 || body.ErrorRate != 0.01 || body.P95Ms != 240 {
		t.Errorf("the store's numbers did not survive: %+v", body)
	}
	// The window that comes back is the one the rollup answered, snapped to its
	// own resolution — not an echo of what was asked for.
	if !body.Since.Equal(answered) || body.Rollup != clickhouse.RequestRollupMinute {
		t.Errorf("expected the answered window and the rollup that answered it, got %+v", body)
	}
	if body.Environment != testEnvironment || !body.Edge.Routed || body.Edge.Message != "" {
		t.Errorf("an environment with a route is on the edge, plainly: %+v", body)
	}
	// The read is project-scoped, and the project is the environment's own.
	asked := h.logs.lastRequestSummary
	if asked.Project != feedProject || asked.Environment != testEnvironment || asked.Route != checkout {
		t.Errorf("the query did not carry the scope: %+v", asked)
	}
}

// An environment nothing publishes is not an environment with no traffic. The
// screen says a different sentence about each, so the answer distinguishes
// them: the numbers alone cannot, because both are zero.
func TestAnEnvironmentOffTheEdgeSaysSoRatherThanDrawingEmptyCharts(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.Edge.Routed {
		t.Fatalf("no HTTPRoute exists, so nothing reaches this environment: %+v", body.Edge)
	}
	if !strings.Contains(body.Edge.Message, "through the platform's edge") {
		t.Errorf("the message should be the one the screen shows: %q", body.Edge.Message)
	}

	// The same environment, published, with a window nothing happened in: on
	// the edge, and quiet. That is the other sentence.
	h = newHarness(t, nil, onTheEdge()...)
	res = h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	quiet := decode[requestSummaryBody](t, res)
	if !quiet.Edge.Routed || quiet.Requests != 0 {
		t.Errorf("expected a routed environment that served nothing, got %+v", quiet)
	}
}

func TestRequestSeries(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	h.logs.requestSeries = clickhouse.RequestSeries{
		BucketSeconds: 300,
		Rollup:        clickhouse.RequestRollupMinute,
		Points: []clickhouse.RequestPoint{
			{Start: time.Now().Add(-5 * time.Minute), Requests: 12, Errors: 1, P95Ms: 88},
		},
	}

	res := h.do(t, http.MethodGet, seriesPath+"?buckets=30", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", seriesPath, res.Code, res.Body.String())
	}
	body := decode[requestSeriesBody](t, res)
	if body.BucketSeconds != 300 || len(body.Points) != 1 || body.Points[0].P95Ms != 88 {
		t.Errorf("the series did not survive: %+v", body)
	}
	if h.logs.lastRequestSeries.Buckets != 30 || h.logs.lastRequestSeries.Environment != testEnvironment {
		t.Errorf("the query did not carry the resolution and the scope: %+v", h.logs.lastRequestSeries)
	}
}

func TestRequestRoutes(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	h.logs.requestRoutes = []clickhouse.RequestRoute{
		{Route: checkout, Requests: 400, Errors: 4, ErrorRate: 0.01, P95Ms: 310},
	}

	res := h.do(t, http.MethodGet, routesPath+"?sort="+clickhouse.RouteSortLatency+"&limit=10", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", routesPath, res.Code, res.Body.String())
	}
	body := decode[requestRoutesBody](t, res)
	if len(body.Items) != 1 || body.Items[0].Route != checkout || body.Items[0].P95Ms != 310 {
		t.Errorf("the route table did not survive: %+v", body.Items)
	}
	// The sort decides which rows survive the limit, so it is a query and has
	// to reach the store.
	if h.logs.lastRequestRoutes.SortBy != clickhouse.RouteSortLatency || h.logs.lastRequestRoutes.Limit != 10 {
		t.Errorf("the sort and the limit did not reach the store: %+v", h.logs.lastRequestRoutes)
	}
}

// A sort nobody offers is the caller's to fix, so it is a 400 that names the
// choices — not a read the caller is told failed.
func TestAnUnknownRouteSortNamesTheChoices(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)

	res := h.do(t, http.MethodGet, routesPath+"?sort=slowest", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("GET %s?sort=slowest = %d: %s", routesPath, res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), clickhouse.RouteSortErrorRate) {
		t.Errorf("the answer should list the sorts: %s", res.Body.String())
	}
}

func TestRequestListFilters(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	h.logs.requests = []clickhouse.Request{{
		Timestamp:  time.Now(),
		Method:     http.MethodGet,
		Path:       "/checkout/9182",
		Route:      checkout,
		Status:     503,
		DurationMs: 12.5,
	}}

	res := h.do(t, http.MethodGet, requestsPath+"?route="+url.QueryEscape(checkout)+
		"&method=get&status=5xx&errors=1&limit=25", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", requestsPath, res.Code, res.Body.String())
	}
	body := decode[requestListBody](t, res)
	if len(body.Items) != 1 || body.Items[0].Path != "/checkout/9182" {
		t.Errorf("the rows did not survive: %+v", body.Items)
	}
	asked := h.logs.lastRequests
	// The verb is canonicalised at ingest, so `?method=get` has to reach the
	// store as the spelling the rows carry.
	if asked.Method != http.MethodGet || asked.StatusClass != 5 || !asked.OnlyErrors {
		t.Errorf("the filters did not reach the store: %+v", asked)
	}
	if asked.Route != checkout || asked.Limit != 25 || asked.Project != feedProject {
		t.Errorf("the scope and the limit did not reach the store: %+v", asked)
	}
}

func TestAStatusFilterIsAClassOfAnswer(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)

	// Both spellings of the same question.
	for _, status := range []string{"4xx", "4"} {
		if res := h.do(t, http.MethodGet, requestsPath+"?status="+status, ""); res.Code != http.StatusOK {
			t.Fatalf("status=%s = %d: %s", status, res.Code, res.Body.String())
		}
		if h.logs.lastRequests.StatusClass != 4 {
			t.Errorf("status=%s did not reach the store as a class: %+v", status, h.logs.lastRequests)
		}
	}
	res := h.do(t, http.MethodGet, requestsPath+"?status=418", "")
	if res.Code != http.StatusBadRequest {
		t.Errorf("one exact code is not offered, and saying so is a 400: %d", res.Code)
	}
}

// A window that ends before it starts is the caller's mistake, and the store
// would only report it as a read that failed.
func TestAnInvertedWindowIsTheCallersProblem(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)

	res := h.do(t, http.MethodGet, summaryPath+"?since=2026-08-16T10:00:00Z&until=2026-08-16T09:00:00Z", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "until must be after since") {
		t.Errorf("the answer should say what is wrong: %s", res.Body.String())
	}
}

// The request reads are the operator's own queries, so a store that refuses one
// is a Kitchen fault: the caller gets the name of the read and nothing of
// ClickHouse's internals.
func TestARefusedRequestReadNamesTheRead(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	h.logs.requestErr = errors.New("clickhouse returned 500 Internal Server Error: Code: 47. " +
		"DB::Exception: Unknown expression identifier `duration` (version 26.3.17.110 (official build))")

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, "the request summary query failed") {
		t.Errorf("the answer should name the read that failed: %s", body)
	}
	if strings.Contains(body, "DB::Exception") {
		t.Errorf("the store's internals reached the browser: %s", body)
	}
}

// The live tail is the log endpoints' loop, over rows instead of lines: the
// same Accept negotiation, the same events, and a plain GET still answers the
// bounded page.
func TestRequestsStreamAsServerSentEvents(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)
	h.logs.requests = []clickhouse.Request{
		// Newest first, the way the listing answers; the tail sends them the
		// other way round, because its boundary only moves forwards.
		{Timestamp: time.Now(), Method: http.MethodGet, Path: "/checkout/2", Status: 500},
		{Timestamp: time.Now().Add(-time.Second), Method: http.MethodGet, Path: "/checkout/1", Status: 200},
	}

	req := httptest.NewRequest(http.MethodGet, requestsPath, nil)
	req.Header.Set("Authorization", "Bearer "+h.issuer.token(t))
	req.Header.Set("Accept", eventStream)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handler.ServeHTTP(recorder, req)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := recorder.Header().Get("Content-Type"); got != eventStream {
		t.Fatalf("Content-Type = %q, want text/event-stream (body: %s)", got, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"timestamp"`) {
		t.Fatalf("expected the rows as SSE events, got:\n%s", body)
	}
	if strings.Index(body, "/checkout/1") > strings.Index(body, "/checkout/2") {
		t.Errorf("the tail sends oldest first, or its boundary drops every row but the newest:\n%s", body)
	}
}

// probed is the fixtures with the project declaring an HTTP health check, which
// is the only thing that makes a route a health check as far as these endpoints
// are concerned.
func probed(path string) []runtime.Object {
	objects := onTheEdge()
	for _, object := range objects {
		if project, ok := object.(*kitchenv1alpha1.Project); ok {
			project.Spec.Runtime.Health = &kitchenv1alpha1.HealthSpec{Path: path}
		}
	}
	return objects
}

// The platform's own checks are not the application's traffic. Every read on
// this screen drops them, and every answer says it did — a number that silently
// left rows out is a number nobody can reconcile against the store.
func TestAHealthCheckIsNotTraffic(t *testing.T) {
	h := newHarness(t, nil, probed("/api/health")...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.HealthChecks.Route != "/api/health" || !body.HealthChecks.Excluded {
		t.Errorf("the answer should name the check it left out: %+v", body.HealthChecks)
	}
	want := []clickhouse.HealthRoute{{Project: feedProject, Route: "/api/health"}}
	if !slices.Equal(h.logs.lastRequestSummary.ExcludeHealth, want) {
		t.Errorf("the summary did not exclude it: %+v", h.logs.lastRequestSummary)
	}

	if res := h.do(t, http.MethodGet, seriesPath, ""); res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", seriesPath, res.Code, res.Body.String())
	}
	if !slices.Equal(h.logs.lastRequestSeries.ExcludeHealth, want) {
		t.Errorf("the series did not exclude it: %+v", h.logs.lastRequestSeries)
	}
	if res := h.do(t, http.MethodGet, routesPath, ""); res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", routesPath, res.Code, res.Body.String())
	}
	if !slices.Equal(h.logs.lastRequestRoutes.ExcludeHealth, want) {
		t.Errorf("the route table did not exclude it: %+v", h.logs.lastRequestRoutes)
	}
	// The rows under the numbers are the traffic those numbers are of.
	if res := h.do(t, http.MethodGet, requestsPath, ""); res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", requestsPath, res.Code, res.Body.String())
	}
	if !slices.Equal(h.logs.lastRequests.ExcludeHealth, want) {
		t.Errorf("the listing did not exclude it: %+v", h.logs.lastRequests)
	}
}

// The exclusion is matched against the stored `route`, which is what the
// follower templated the path to — so a health path carrying an id has to be
// templated by the same code on the way in and on the way out.
func TestAHealthCheckIsExcludedAsTheRouteItWasTemplatedTo(t *testing.T) {
	h := newHarness(t, nil, probed("/api/health/12345")...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	if got := decode[requestSummaryBody](t, res).HealthChecks.Route; got != "/api/health/:id" {
		t.Errorf("health route = %q, want the template the rows carry", got)
	}
}

// The older question is still askable, and it is one parameter.
func TestHealthChecksCanBeCountedBackIn(t *testing.T) {
	h := newHarness(t, nil, probed("/api/health")...)

	res := h.do(t, http.MethodGet, summaryPath+"?health=include", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	// The route is still named: it is what the screen offers to exclude again.
	if body.HealthChecks.Route != "/api/health" || body.HealthChecks.Excluded {
		t.Errorf("nothing should have been excluded: %+v", body.HealthChecks)
	}
	if h.logs.lastRequestSummary.ExcludeHealth != nil {
		t.Errorf("the store was still asked to exclude: %+v", h.logs.lastRequestSummary)
	}
}

// A caller who filtered to the health route asked for it, and answering zero
// would be the screen arguing with itself.
func TestFilteringToTheHealthRouteCountsIt(t *testing.T) {
	h := newHarness(t, nil, probed("/api/health")...)

	res := h.do(t, http.MethodGet, summaryPath+"?route="+url.QueryEscape("/api/health"), "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.HealthChecks.Excluded {
		t.Errorf("the numbers are of the route that was asked for: %+v", body.HealthChecks)
	}
	if h.logs.lastRequestSummary.ExcludeHealth != nil {
		t.Errorf("a named route should not also be excluded: %+v", h.logs.lastRequestSummary)
	}
}

// A project that declared no HTTP check has nothing to exclude, and says so
// rather than leaving the screen to guess what the empty route means.
func TestAProjectWithNoHealthCheckExcludesNothing(t *testing.T) {
	h := newHarness(t, nil, onTheEdge()...)

	res := h.do(t, http.MethodGet, summaryPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", summaryPath, res.Code, res.Body.String())
	}
	body := decode[requestSummaryBody](t, res)
	if body.HealthChecks.Route != "" || body.HealthChecks.Excluded {
		t.Errorf("nothing was declared, so nothing is excluded: %+v", body.HealthChecks)
	}
	if h.logs.lastRequestSummary.ExcludeHealth != nil {
		t.Errorf("the store should have been asked for everything: %+v", h.logs.lastRequestSummary)
	}
}

func TestAnUnknownHealthValueNamesTheChoices(t *testing.T) {
	h := newHarness(t, nil, probed("/api/health")...)

	res := h.do(t, http.MethodGet, summaryPath+"?health=maybe", "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "include") || !strings.Contains(res.Body.String(), "exclude") {
		t.Errorf("the answer should name the choices: %s", res.Body.String())
	}
}

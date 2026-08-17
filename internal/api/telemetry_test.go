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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// feedProject is the fixtures' project, whose telemetry these tests read.
const feedProject = "shop"

func TestListEvents(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.events = []clickhouse.Event{{
		Timestamp: time.Now(),
		Type:      clickhouse.EventBuildFailed,
		Project:   feedProject,
		Build:     "shop-bld-abc123def456",
		Message:   "build shop-bld-abc123def456 failed: no Dockerfile",
		Actor:     "operator",
	}}

	res := h.do(t, http.MethodGet, "/api/v1/events?project=shop&limit=10", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /events = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Items []clickhouse.Event `json:"items"`
	}](t, res)
	if len(body.Items) != 1 || body.Items[0].Type != clickhouse.EventBuildFailed {
		t.Errorf("unexpected feed %+v", body.Items)
	}
	if h.logs.lastEvents.Project != feedProject || h.logs.lastEvents.Limit != 10 {
		t.Errorf("query did not carry the parameters: %+v", h.logs.lastEvents)
	}
}

// The per-project numbers are the request pipeline's, not the flow pipeline's.
// Flows are keyed on the destination endpoint, so a protected preview's traffic
// was credited to the forward-auth gate and an idling environment's to the KEDA
// interceptor — both of which live in the platform's namespace, so both simply
// vanished from the project that actually served them.
func TestMetricsOverviewCountsProjectTrafficFromTheRequestPipeline(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.overview = clickhouse.MetricsOverview{
		Requests24h: 120,
		// What the flows saw of this project: four requests, because the gate
		// answered the rest under its own namespace.
		Namespaces: []clickhouse.NamespaceTraffic{{Namespace: "kitchen-shop", Requests24h: 4}},
	}
	h.logs.projectTraffic = []clickhouse.ProjectTraffic{{
		Project:         feedProject,
		Requests:        100,
		Errors:          3,
		P95Ms:           42,
		RequestsPerHour: make([]uint64, 24),
	}}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Requests24h uint64           `json:"requests24h"`
		Projects    []projectTraffic `json:"projects"`
		Namespaces  []any            `json:"namespaces"`
	}](t, res)
	if body.Requests24h != 120 {
		t.Errorf("requests24h = %d, want 120", body.Requests24h)
	}
	if len(body.Projects) != 1 {
		t.Fatalf("expected one project row, got %+v", body.Projects)
	}
	row := body.Projects[0]
	if row.Project != feedProject || row.Requests24h != 100 || row.Errors5xx24h != 3 || row.P95Ms != 42 {
		t.Errorf("the project's row should be the request pipeline's, got %+v", row)
	}
	if body.Namespaces != nil {
		t.Errorf("raw namespace rows should not reach the answer, got %+v", body.Namespaces)
	}

	// The window is whole hours ending with the one in progress, and the
	// sparkline is asked for — a per-project row without one draws no chart.
	asked := h.logs.lastProjectTraffic
	if !asked.Sparkline {
		t.Errorf("the overview needs the hourly series: %+v", asked)
	}
	if !asked.Since.Equal(asked.Since.Truncate(time.Hour)) || time.Since(asked.Since) < 23*time.Hour {
		t.Errorf("expected a window of whole hours covering the last day, got %s", asked.Since)
	}
}

// A project nobody visited still belongs in the table, at zero: the overview is
// a list of projects with numbers on it, not a list of numbers.
func TestMetricsOverviewKeepsProjectsWithNoTraffic(t *testing.T) {
	quiet := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:          "acme/blog",
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), quiet)...)
	h.logs.projectTraffic = []clickhouse.ProjectTraffic{{Project: feedProject, Requests: 100}}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Projects []projectTraffic `json:"projects"`
	}](t, res)
	if len(body.Projects) != 2 || body.Projects[0].Project != "blog" {
		t.Fatalf("expected both projects, sorted by name, got %+v", body.Projects)
	}
	if body.Projects[0].Requests24h != 0 || len(body.Projects[0].RequestsPerHour) != 24 {
		t.Errorf("a project with no traffic wants a row of zeroes and a flat sparkline, got %+v", body.Projects[0])
	}
}

func TestMetricsOverviewScopedToAProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview?project=shop", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview?project=shop = %d: %s", res.Code, res.Body.String())
	}
	if h.logs.lastMetrics.Project != feedProject || h.logs.lastMetrics.Namespace != "kitchen-shop" {
		t.Errorf("expected the store query scoped to the project and its namespace, got %+v", h.logs.lastMetrics)
	}

	// A typo answers 404, not zeroes.
	if res := h.do(t, http.MethodGet, "/api/v1/metrics/overview?project=nope", ""); res.Code != http.StatusNotFound {
		t.Errorf("GET /metrics/overview?project=nope = %d, want 404", res.Code)
	}
}

func TestTraffic(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.edges = []clickhouse.TrafficEdge{{
		Source: "gateway", Destination: testEnvironment,
		DestinationNamespace: "kitchen-shop", Protocol: "HTTP",
		Flows: 100, RPS: 1.5, Errors: 2, P95Ms: 40,
	}}

	res := h.do(t, http.MethodGet, "/api/v1/traffic?project=shop", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /traffic = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Items []clickhouse.TrafficEdge `json:"items"`
	}](t, res)
	if len(body.Items) != 1 || body.Items[0].Destination != testEnvironment {
		t.Errorf("unexpected edges %+v", body.Items)
	}
	if h.logs.lastTraffic.Namespace != "kitchen-shop" {
		t.Errorf("expected the query scoped to the project's namespace, got %+v", h.logs.lastTraffic)
	}
}

// The traffic query is the operator's, so ClickHouse refusing it is a Kitchen
// fault and not a caller's mistake: the browser was shown a raw
// `Code: 184. DB::Exception: … (version 26.3.17.110 (official build))`, which
// tells the person reading it nothing they can do.
func TestAStoreFailureDoesNotReachTheBrowser(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.trafficErr = errors.New("clickhouse returned 500 Internal Server Error: Code: 184. " +
		"DB::Exception: Aggregate function countIf(protocol = 'HTTP') is found inside another " +
		"aggregate function in query. (ILLEGAL_AGGREGATION) (version 26.3.17.110 (official build))")

	res := h.do(t, http.MethodGet, "/api/v1/traffic", "")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("GET /traffic = %d: %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	if !strings.Contains(body, "the traffic query failed") {
		t.Errorf("the answer should name the read that failed: %s", body)
	}
	for _, leak := range []string{"DB::Exception", "ILLEGAL_AGGREGATION", "official build"} {
		if strings.Contains(body, leak) {
			t.Errorf("the store's internals reached the browser (%q): %s", leak, body)
		}
	}
}

func TestLogsStreamAsServerSentEvents(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.lines = []clickhouse.LogLine{
		{Timestamp: time.Now(), Source: "build", Build: "shop-bld-abc123def456", Message: "step 1/4"},
		{Timestamp: time.Now(), Source: "build", Build: "shop-bld-abc123def456", Message: "step 2/4"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/builds/shop-bld-abc123def456/logs", nil)
	req.Header.Set("Authorization", "Bearer "+h.issuer.token(t))
	req.Header.Set("Accept", "text/event-stream")
	// The stream follows until the client goes away; a cancelled context is
	// the client going away. The deadline is only a backstop so a broken
	// stream cannot hang the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.handler.ServeHTTP(recorder, req)
	}()
	// The initial page is sent immediately; ending the request ends the loop.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream (body: %s)", got, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"timestamp"`) || !strings.Contains(body, "step 2/4") {
		t.Errorf("expected the initial lines as SSE events, got:\n%s", body)
	}
}

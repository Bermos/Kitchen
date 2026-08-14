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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestMetricsOverviewJoinsProjects(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.overview = clickhouse.MetricsOverview{
		Requests24h: 120,
		Namespaces: []clickhouse.NamespaceTraffic{
			// The project's app namespace, and one that belongs to no project.
			{Namespace: "kitchen-shop", Requests24h: 100, Errors5xx24h: 3},
			{Namespace: "kube-system", Requests24h: 20},
		},
	}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Requests24h uint64 `json:"requests24h"`
		Projects    []struct {
			Project     string `json:"project"`
			Requests24h uint64 `json:"requests24h"`
		} `json:"projects"`
		Namespaces []any `json:"namespaces"`
	}](t, res)
	if body.Requests24h != 120 {
		t.Errorf("requests24h = %d, want 120", body.Requests24h)
	}
	if len(body.Projects) != 1 || body.Projects[0].Project != feedProject || body.Projects[0].Requests24h != 100 {
		t.Errorf("expected the shop project's traffic joined in, got %+v", body.Projects)
	}
	if body.Namespaces != nil {
		t.Errorf("raw namespace rows should not reach the answer, got %+v", body.Namespaces)
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
		Source: "gateway", Destination: "shop-production",
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
	if len(body.Items) != 1 || body.Items[0].Destination != "shop-production" {
		t.Errorf("unexpected edges %+v", body.Items)
	}
	if h.logs.lastTraffic.Namespace != "kitchen-shop" {
		t.Errorf("expected the query scoped to the project's namespace, got %+v", h.logs.lastTraffic)
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

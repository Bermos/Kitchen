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

// Every traffic number on the overview is the request pipeline's, not the flow
// pipeline's — the totals as well as the per-project rows. Flows are keyed on
// the destination endpoint, so a protected preview's traffic was credited to
// the forward-auth gate and an idling environment's to the KEDA interceptor;
// both live in the platform's namespace, so both vanished from the project that
// served them and swelled the platform's own numbers instead.
func TestMetricsOverviewCountsTrafficFromTheRequestPipeline(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	// The traffic fields the store still carries on this struct, and what none
	// of them may reach. Nothing fills them any more — the flow aggregations
	// that used to are gone — but the fields stay on the wire, so a value
	// arriving in them must still be overwritten rather than merged.
	h.logs.overview = clickhouse.MetricsOverview{
		Requests24h:     120,
		ErrorRate24h:    0.5,
		P95Ms24h:        900,
		RequestsPerHour: repeated(24, 5),
	}
	h.logs.platformRequests = clickhouse.PlatformRequests{Requests: 900, ErrorRate: 0.02, P95Ms: 42}
	// One hour of the day had traffic, and its p95 is the hour's own — not the
	// day's, and not an average of anything.
	hour := time.Now().UTC().Truncate(time.Hour).Hour()
	h.logs.platformHours = map[int]clickhouse.PlatformRequests{
		hour: {Requests: 300, Errors: 6, P95Ms: 77},
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
		Requests24h     uint64           `json:"requests24h"`
		ErrorRate24h    float64          `json:"errorRate24h"`
		P95Ms24h        float64          `json:"p95Ms24h"`
		RequestsPerHour []uint64         `json:"requestsPerHour"`
		ErrorsPerHour   []uint64         `json:"errorsPerHour"`
		P95MsPerHour    []float64        `json:"p95MsPerHour"`
		Projects        []projectTraffic `json:"projects"`
		Namespaces      []any            `json:"namespaces"`
	}](t, res)

	if body.Requests24h != 900 || body.ErrorRate24h != 0.02 || body.P95Ms24h != 42 {
		t.Errorf("the totals should be the platform read's, got %d / %v / %v",
			body.Requests24h, body.ErrorRate24h, body.P95Ms24h)
	}
	if len(body.RequestsPerHour) != 24 || len(body.ErrorsPerHour) != 24 || len(body.P95MsPerHour) != 24 {
		t.Fatalf("the sparklines are 24 hours long: %+v", body)
	}
	// The last bucket is the hour in progress, which is the one the fixture
	// filled — and nothing of the flow pipeline's flat five survives.
	if body.RequestsPerHour[23] != 300 || body.ErrorsPerHour[23] != 6 || body.P95MsPerHour[23] != 77 {
		t.Errorf("the hour in progress should carry its own numbers: %+v", body)
	}
	if body.RequestsPerHour[0] != 0 {
		t.Errorf("an hour the request pipeline did not fill must not keep the flow pipeline's: %+v",
			body.RequestsPerHour)
	}

	if len(body.Projects) != 1 {
		t.Fatalf("expected one project row, got %+v", body.Projects)
	}
	row := body.Projects[0]
	if row.Project != feedProject || row.Requests24h != 100 || row.Errors5xx24h != 3 || row.P95Ms != 42 {
		t.Errorf("the project's row should be the request pipeline's, got %+v", row)
	}
	// The per-namespace flow rows were the last reader of an attribution this
	// endpoint no longer uses, and the store stopped computing them: the key
	// must be absent from the answer entirely.
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
	// One day-wide read and one per hour: the percentile series cannot be
	// derived from the day's answer, which is the reason there are 25 of them.
	if len(h.logs.platformQueries) != 1+overviewHours {
		t.Errorf("expected the day and its 24 hours, got %d reads", len(h.logs.platformQueries))
	}
}

// repeated is a series of one number, for the flow pipeline's answer that must
// not survive the switch.
func repeated(count int, value uint64) []uint64 {
	series := make([]uint64, count)
	for i := range series {
		series[i] = value
	}
	return series
}

// `?project=` narrows the same correction to one project, where the store has a
// read shaped for it: a summary and a series rather than a day of hours.
func TestMetricsOverviewScopedToAProjectReadsTheRequestPipeline(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.overview = clickhouse.MetricsOverview{Requests24h: 120, P95Ms24h: 900}
	h.logs.requestSummary = clickhouse.RequestSummary{Requests: 100, ErrorRate: 0.03, P95Ms: 42}
	start := time.Now().UTC().Truncate(time.Hour).Add(-23 * time.Hour)
	h.logs.requestSeries = clickhouse.RequestSeries{
		BucketSeconds: 3600,
		Points: []clickhouse.RequestPoint{
			{Start: start, Requests: 10, Errors: 1, P95Ms: 30},
			{Start: start.Add(23 * time.Hour), Requests: 90, Errors: 2, P95Ms: 50},
		},
	}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview?project=shop", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview?project=shop = %d: %s", res.Code, res.Body.String())
	}
	body := decode[struct {
		Requests24h     uint64    `json:"requests24h"`
		P95Ms24h        float64   `json:"p95Ms24h"`
		RequestsPerHour []uint64  `json:"requestsPerHour"`
		P95MsPerHour    []float64 `json:"p95MsPerHour"`
	}](t, res)

	if body.Requests24h != 100 || body.P95Ms24h != 42 {
		t.Errorf("the project's totals should be its own rollup's, got %d / %v", body.Requests24h, body.P95Ms24h)
	}
	if body.RequestsPerHour[0] != 10 || body.RequestsPerHour[23] != 90 || body.P95MsPerHour[23] != 50 {
		t.Errorf("the series should land on the hours it reported: %+v", body)
	}
	if h.logs.lastRequestSummary.Project != feedProject || h.logs.lastRequestSummary.Environment != "" {
		t.Errorf("the read is the whole project's: %+v", h.logs.lastRequestSummary)
	}
	// The platform read is for the platform. A project's numbers never come
	// from it, or one project's page would report another's traffic.
	if len(h.logs.platformQueries) != 0 {
		t.Errorf("a project-scoped overview should not read across projects: %+v", h.logs.platformQueries)
	}
}

// A project nobody visited still belongs in the table, at zero: the overview is
// a list of projects with numbers on it, not a list of numbers.
func TestMetricsOverviewKeepsProjectsWithNoTraffic(t *testing.T) {
	quiet := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: otherProject, Namespace: testNamespace},
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
	if len(body.Projects) != 2 || body.Projects[0].Project != otherProject {
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
	// Project is the only scope left: the traffic half of this answer comes off
	// the request pipeline, which is keyed on the project rather than on the
	// destination namespace the flows were attributed by.
	if h.logs.lastMetrics.Project != feedProject {
		t.Errorf("expected the store query scoped to the project, got %+v", h.logs.lastMetrics)
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
	req.Header.Set("Accept", eventStream)
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

	if got := recorder.Header().Get("Content-Type"); got != eventStream {
		t.Fatalf("Content-Type = %q, want text/event-stream (body: %s)", got, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"timestamp"`) || !strings.Contains(body, "step 2/4") {
		t.Errorf("expected the initial lines as SSE events, got:\n%s", body)
	}
}

// `/metrics/overview` is a cross-project read, and every one of those is
// answered about the caller's own projects. A member holding nothing must not
// be told how much the platform deployed, served or stored — and the store
// must not be asked, because there is nothing of theirs to ask about.
func TestTheMetricsOverviewOfACallerWithNoProjectsIsZeroesAndNoStoreRead(t *testing.T) {
	h := asMember(t, "")
	h.logs.overview = clickhouse.MetricsOverview{
		Deploys7d:          9,
		MedianBuildSeconds: 30,
		LogLines24h:        5000,
		StoreBytes:         1 << 40,
		StoreRowsPerSecond: 12,
	}
	h.logs.platformRequests = clickhouse.PlatformRequests{Requests: 900, ErrorRate: 0.02, P95Ms: 42}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[metricsOverviewBody](t, res)
	if body.Deploys7d != 0 || body.LogLines24h != 0 || body.Requests24h != 0 ||
		body.MedianBuildSeconds != 0 || body.P95Ms24h != 0 ||
		body.StoreBytes != 0 || body.StoreRowsPerSecond != 0 {
		t.Fatalf("the platform's numbers are not a member's to read: %+v", body.MetricsOverview)
	}
	// Zeroes, but the right shape: a sparkline is a flat day rather than a
	// missing one.
	if len(body.RequestsPerHour) != 24 || len(body.LogLinesPerHour) != 24 || len(body.DeploysPerDay) != 7 {
		t.Errorf("the series keep the widths the dashboard draws: %+v", body.MetricsOverview)
	}
	if h.logs.overviewReads != 0 || len(h.logs.platformQueries) != 0 {
		t.Errorf("nothing of theirs to aggregate is nothing to ask the store: %d overview reads, %d platform reads",
			h.logs.overviewReads, len(h.logs.platformQueries))
	}
}

// A member on one project is answered about that project — the same reads
// `?project=` makes — and never through the platform-wide rollup.
func TestTheMetricsOverviewOfAMemberIsScopedToTheirProjects(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	h.logs.overview = clickhouse.MetricsOverview{Deploys7d: 4, MedianBuildSeconds: 30}
	h.logs.requestSummary = clickhouse.RequestSummary{Requests: 100, ErrorRate: 0.03, P95Ms: 42}
	h.logs.platformRequests = clickhouse.PlatformRequests{Requests: 900, ErrorRate: 0.02, P95Ms: 99}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[metricsOverviewBody](t, res)

	if len(h.logs.platformQueries) != 0 {
		t.Errorf("a member's overview must not read across projects: %+v", h.logs.platformQueries)
	}
	if h.logs.lastMetrics.Project != feedProject {
		t.Errorf("want the store asked about %s, got %+v", feedProject, h.logs.lastMetrics)
	}
	if h.logs.lastRequestSummary.Project != feedProject {
		t.Errorf("want the traffic read scoped to %s, got %+v", feedProject, h.logs.lastRequestSummary)
	}
	// One project is nothing to merge, so its own percentile and median are
	// the answer rather than being dropped.
	if body.Requests24h != 100 || body.P95Ms24h != 42 || body.MedianBuildSeconds != 30 {
		t.Errorf("want the project's own numbers, got %+v", body.MetricsOverview)
	}
}

// On several projects the counts add up and the numbers that cannot be merged
// from per-project answers are left at zero rather than invented: a mean of
// p95s is not a p95, and the honest per-project ones are in `projects`.
func TestTheMetricsOverviewMergesTheCountsAndNotThePercentiles(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer, blogFixtures()...)
	h.grant(t, otherProject, kitchenv1alpha1.AccessRoleViewer)
	h.logs.overview = clickhouse.MetricsOverview{
		Deploys7d:          4,
		DeploysPerDay:      repeated(7, 1),
		MedianBuildSeconds: 30,
		LogLines24h:        100,
		StoreBytes:         2048,
		StoreRowsPerSecond: 8,
	}
	h.logs.requestSummary = clickhouse.RequestSummary{Requests: 100, ErrorRate: 0.5, P95Ms: 42}

	res := h.do(t, http.MethodGet, "/api/v1/metrics/overview", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET /metrics/overview = %d: %s", res.Code, res.Body.String())
	}
	body := decode[metricsOverviewBody](t, res)

	if body.Deploys7d != 8 || body.LogLines24h != 200 || body.Requests24h != 200 {
		t.Errorf("counts over two projects add up, got %+v", body.MetricsOverview)
	}
	if body.DeploysPerDay[0] != 2 {
		t.Errorf("the buckets add up bucket by bucket, got %+v", body.DeploysPerDay)
	}
	// The rate is the pooled one — total errors over total requests — not a
	// sum of rates.
	if body.ErrorRate24h != 0.5 {
		t.Errorf("want the pooled error rate, got %v", body.ErrorRate24h)
	}
	if body.P95Ms24h != 0 || body.MedianBuildSeconds != 0 {
		t.Errorf("a percentile merged out of per-project percentiles would be a made-up number: %+v",
			body.MetricsOverview)
	}
	// The store's own size is the store's, and is reported once rather than
	// once per project.
	if body.StoreBytes != 2048 || body.StoreRowsPerSecond != 8 {
		t.Errorf("the store's own figures are not per-project sums, got %+v", body.MetricsOverview)
	}
}

// The operator's answer is unchanged: the platform, whole.
func TestTheMetricsOverviewAnswersTheOperatorAboutThePlatform(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.overview = clickhouse.MetricsOverview{Deploys7d: 9}
	h.logs.platformRequests = clickhouse.PlatformRequests{Requests: 900, ErrorRate: 0.02, P95Ms: 42}

	body := decode[metricsOverviewBody](t, h.do(t, http.MethodGet, "/api/v1/metrics/overview", ""))
	if body.Deploys7d != 9 || body.Requests24h != 900 || body.P95Ms24h != 42 {
		t.Errorf("an operator's overview is the platform's, got %+v", body.MetricsOverview)
	}
	if h.logs.lastMetrics.Project != "" || len(h.logs.platformQueries) == 0 {
		t.Errorf("the operator's read is the platform-wide one: %+v / %+v", h.logs.lastMetrics, h.logs.platformQueries)
	}
}

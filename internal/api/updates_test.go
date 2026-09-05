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

	"github.com/blang/semver/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/chartrepo"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// runningVersion is what these tests' installation is on: every offer, refusal
// and ordering below is relative to it.
const runningVersion = "0.2.0"

// stubCharts stands in for the registry, so the endpoint's arithmetic can be
// tested without one. It counts the two ways it can be read separately: which
// one a request took is the whole of what `?refresh=` decides.
type stubCharts struct {
	versions []string
	err      error

	cached    int
	refreshed int
	checkedAt time.Time
}

func (s *stubCharts) Versions(context.Context) (chartrepo.Listing, error) {
	s.cached++
	return s.listing()
}

func (s *stubCharts) Refresh(context.Context) (chartrepo.Listing, error) {
	s.refreshed++
	return s.listing()
}

func (s *stubCharts) listing() (chartrepo.Listing, error) {
	checked := s.checkedAt
	if checked.IsZero() {
		checked = time.Now()
	}
	if s.err != nil {
		return chartrepo.Listing{CheckedAt: checked}, s.err
	}
	out := make([]semver.Version, 0, len(s.versions))
	for _, version := range s.versions {
		out = append(out, semver.MustParse(version))
	}
	return chartrepo.Listing{Versions: out, CheckedAt: checked}, nil
}

// enabledHarness is an installation that was upgraded with selfUpdate.enabled.
func enabledHarness(t *testing.T, published ...string) *harness {
	t.Helper()
	h := newHarness(t, nil)
	h.server.Version = runningVersion
	h.server.SelfUpdate = controller.SelfUpdateConfig{
		Chart:          "oci://ghcr.io/bermos/charts/kitchen",
		Release:        "kitchen",
		ServiceAccount: "kitchen-self-update",
	}
	if len(published) > 0 {
		h.server.charts = &stubCharts{versions: published}
	}
	return h
}

func TestUpdatesReportTheRunningVersionWhenSelfUpdateIsOff(t *testing.T) {
	h := newHarness(t, nil)
	h.server.Version = runningVersion

	recorder := h.do(t, http.MethodGet, "/api/v1/updates", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[updatesView](t, recorder)
	if body.Enabled {
		t.Fatal("self-update is off by default")
	}
	if body.CurrentVersion != runningVersion {
		t.Fatalf("the version it is running is worth answering either way, got %+v", body)
	}
	if !strings.Contains(body.Reason, "selfUpdate.enabled=true") {
		t.Fatalf("want the reason to say how to turn it on, got %q", body.Reason)
	}
}

func TestUpdatesOfferOnlyWhatTheOperatorWouldAccept(t *testing.T) {
	h := enabledHarness(t, "0.1.4", runningVersion, "0.2.1", "0.2.2", "0.3.0", "0.3.1-rc.1")

	body := decode[updatesView](t, h.do(t, http.MethodGet, "/api/v1/updates", ""))
	if !body.Enabled || !body.Available {
		t.Fatalf("want an available upgrade, got %+v", body)
	}
	if body.LatestVersion != "0.3.0" {
		t.Fatalf("want the newest stable release, got %q", body.LatestVersion)
	}
	// allowMinor is off, so 0.3.0 is reported as the latest but not offered:
	// pre-1.0 the minor is where breaking changes land.
	if strings.Join(body.UpgradableTo, " ") != "0.2.2 0.2.1" {
		t.Fatalf("want the patch releases newest first and nothing else, got %v", body.UpgradableTo)
	}
}

func TestUpdatesOfferMinorsWhenTheChartAllowsThem(t *testing.T) {
	h := enabledHarness(t, runningVersion, "0.2.1", "0.3.0")
	h.server.SelfUpdate.AllowMinor = true

	body := decode[updatesView](t, h.do(t, http.MethodGet, "/api/v1/updates", ""))
	if strings.Join(body.UpgradableTo, " ") != "0.3.0 0.2.1" {
		t.Fatalf("want the minor offered too, got %v", body.UpgradableTo)
	}
	if !body.AllowMinor {
		t.Fatal("the dashboard has to be able to say which kind of upgrade it is offering")
	}
}

func TestUpdatesStillAnswerWhenTheRegistryCannotBeReached(t *testing.T) {
	h := enabledHarness(t)
	h.server.charts = &stubCharts{err: errors.New("cannot reach ghcr.io: no route to host")}

	recorder := h.do(t, http.MethodGet, "/api/v1/updates", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("an unreachable registry must not break the settings page, got %d", recorder.Code)
	}
	body := decode[updatesView](t, recorder)
	if body.CurrentVersion != runningVersion || !body.Enabled {
		t.Fatalf("want the local half of the answer intact, got %+v", body)
	}
	if !strings.Contains(body.DiscoveryError, "no route to host") {
		t.Fatalf("want the reason the versions are missing, got %q", body.DiscoveryError)
	}
}

func TestRecheckingAsksTheRegistryRatherThanTheCache(t *testing.T) {
	h := enabledHarness(t, runningVersion, "0.2.1")
	charts := h.server.charts.(*stubCharts)

	h.do(t, http.MethodGet, "/api/v1/updates", "")
	if charts.cached != 1 || charts.refreshed != 0 {
		t.Fatalf("want an ordinary read served from the cache, got %d cached and %d forced",
			charts.cached, charts.refreshed)
	}

	body := decode[updatesView](t, h.do(t, http.MethodGet, "/api/v1/updates?refresh=true", ""))
	if charts.refreshed != 1 {
		t.Fatalf("want ?refresh=true to reach the registry, it read the cache %d more times", charts.cached-1)
	}
	// Without it the control is indistinguishable from one that did nothing:
	// a release published a minute ago and one published an hour ago produce
	// the same list.
	if body.CheckedAt == nil {
		t.Fatal("want the answer to say when it was taken")
	}
}

func TestAnUnreadableRefreshFlagIsRefused(t *testing.T) {
	h := enabledHarness(t, runningVersion, "0.2.1")

	recorder := h.do(t, http.MethodGet, "/api/v1/updates?refresh=please", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want a caller who meant something told so, got %d", recorder.Code)
	}
	if charts := h.server.charts.(*stubCharts); charts.cached+charts.refreshed != 0 {
		t.Fatal("a refused request must not reach the registry at all")
	}
}

func TestRequestingAnUpdate(t *testing.T) {
	h := enabledHarness(t, runningVersion, "0.2.1")

	recorder := h.do(t, http.MethodPost, "/api/v1/updates", `{"version": "0.2.1"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[updateView](t, recorder)
	if body.Version != "0.2.1" {
		t.Fatalf("want the requested version, got %+v", body)
	}

	updates := &kitchenv1alpha1.PlatformUpdateList{}
	if err := h.server.Client.List(context.Background(), updates); err != nil {
		t.Fatal(err)
	}
	if len(updates.Items) != 1 || updates.Items[0].Spec.Version != "0.2.1" {
		t.Fatalf("want one PlatformUpdate for 0.2.1, got %+v", updates.Items)
	}
	if updates.Items[0].Annotations[requestedByAnnotation] == "" {
		t.Fatal("an upgrade of the platform should record who asked for it")
	}
}

func TestRequestingAnUpdateTakesNothingButAVersion(t *testing.T) {
	h := enabledHarness(t, runningVersion, "0.2.1")

	// The job that runs the upgrade holds cluster-admin, so an endpoint that
	// quietly accepted extra helm arguments would be a way to reach it.
	recorder := h.do(t, http.MethodPost, "/api/v1/updates",
		`{"version": "0.2.1", "helmArgs": ["--set", "image.repository=evil.example.com/kitchen"]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want the unknown field refused, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRequestingAnUpdateRejectsSomethingThatIsNotAVersion(t *testing.T) {
	h := enabledHarness(t, runningVersion)

	for _, version := range []string{"latest", "0.2", "; helm uninstall kitchen", ""} {
		recorder := h.do(t, http.MethodPost, "/api/v1/updates", `{"version": "`+version+`"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want %q refused, got %d: %s", version, recorder.Code, recorder.Body.String())
		}
	}
}

func TestRequestingAnUpdateWhenSelfUpdateIsOff(t *testing.T) {
	h := newHarness(t, nil)
	h.server.Version = runningVersion

	recorder := h.do(t, http.MethodPost, "/api/v1/updates", `{"version": "0.2.1"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}

	updates := &kitchenv1alpha1.PlatformUpdateList{}
	if err := h.server.Client.List(context.Background(), updates); err != nil {
		t.Fatal(err)
	}
	if len(updates.Items) != 0 {
		t.Fatalf("nothing should have been created, got %+v", updates.Items)
	}
}

// The upgrade the log tests read: its name, the job the operator started for
// it — `status.jobName`, which is the whole of what its logs are selected over
// — and the route they are read from.
const (
	testUpdate     = "update-0-2-1-h4k9c"
	testUpdateJob  = "kitchen-self-update-" + testUpdate
	testUpdateLogs = "/api/v1/updates/" + testUpdate + "/logs"
)

// startedUpdate is an upgrade the operator has already reached.
func startedUpdate(name, jobName string) *kitchenv1alpha1.PlatformUpdate {
	return &kitchenv1alpha1.PlatformUpdate{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       kitchenv1alpha1.PlatformUpdateSpec{Version: "0.2.1"},
		Status: kitchenv1alpha1.PlatformUpdateStatus{
			Phase:       kitchenv1alpha1.PlatformUpdateRunning,
			FromVersion: runningVersion,
			JobName:     jobName,
		},
	}
}

func TestReadingAnUpdatesLogs(t *testing.T) {
	h := newHarness(t, nil, startedUpdate(testUpdate, testUpdateJob))
	h.server.Version = runningVersion
	h.logs.lines = []clickhouse.LogLine{
		{Timestamp: time.Now(), Source: clickhouse.SourcePlatform, Message: "Release \"kitchen\" has been upgraded."},
	}

	recorder := h.do(t, http.MethodGet,
		testUpdateLogs+"?limit=50&search=upgraded", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[listBody[clickhouse.LogLine]](t, recorder); len(body.Items) != 1 {
		t.Fatalf("want the store's line, got %d", len(body.Items))
	}

	query := h.logs.lastFilter.Query
	for _, term := range []string{
		"source:" + clickhouse.SourcePlatform,
		"namespace:" + controller.PlatformNamespace,
		"pod:" + testUpdateJob + "-*",
		"container:helm",
		`"upgraded"`,
	} {
		if !strings.Contains(query, term) {
			t.Fatalf("the selection is missing %s: %q", term, query)
		}
	}
	if h.logs.lastFilter.Limit != 50 {
		t.Fatalf("the read dropped its limit: %+v", h.logs.lastFilter)
	}
}

// The route is the operator's, but it reads the platform's own namespace —
// where the API, the operator and the identity provider also write. So the
// selection is composed from the update and cannot be added to: `q` and
// `where` are not parameters of this endpoint, and a caller who sends them
// gets the same lines as one who does not. (`where` is not a parameter of any
// endpoint any more — see query_logs_test.go — but this route ignored it
// rather than refusing it, and still does.)
func TestAnUpdatesLogsCannotBeWidenedByTheCaller(t *testing.T) {
	h := newHarness(t, nil, startedUpdate(testUpdate, testUpdateJob))

	recorder := h.do(t, http.MethodGet,
		testUpdateLogs+"?q=namespace:*&where=1+%3D+1", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !h.logs.lastFilter.Scope.Platform {
		t.Fatalf("the platform's own namespace is read as the platform: %+v", h.logs.lastFilter.Scope)
	}
	if !strings.Contains(h.logs.lastFilter.Query, "pod:"+testUpdateJob+"-*") {
		t.Fatalf("the caller's query replaced the update's own: %q", h.logs.lastFilter.Query)
	}
}

// An upgrade that failed preflight — or that the reconciler has not reached
// yet — never had a job, so there is no pod and nothing to read. That is an
// empty page rather than a 404 or an error: the record itself says what
// happened, and the store must not be asked over a selection with no pod in it.
func TestAnUpdateWithNoJobHasNoLogs(t *testing.T) {
	h := newHarness(t, nil, startedUpdate("update-0-3-0-p2m7x", ""))

	recorder := h.do(t, http.MethodGet, "/api/v1/updates/update-0-3-0-p2m7x/logs", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[listBody[clickhouse.LogLine]](t, recorder); len(body.Items) != 0 {
		t.Fatalf("want no lines, got %+v", body.Items)
	}
	if h.logs.lastFilter.Query != "" {
		t.Fatalf("an update with no pod must not reach the store: %q", h.logs.lastFilter.Query)
	}
}

func TestLogsOfAnUpdateThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/updates/update-nope/logs", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnUpdatesLogsStreamAsServerSentEvents(t *testing.T) {
	h := newHarness(t, nil, startedUpdate(testUpdate, testUpdateJob))
	h.logs.lines = []clickhouse.LogLine{
		{Timestamp: time.Now(), Source: clickhouse.SourcePlatform, Message: "Release \"kitchen\" has been upgraded."},
	}

	req := httptest.NewRequest(http.MethodGet, testUpdateLogs, nil)
	req.Header.Set("Authorization", "Bearer "+h.issuer.token(t))
	req.Header.Set("Accept", eventStream)
	// The tail follows until the client goes away; a cancelled context is the
	// client going away, and the deadline is only a backstop.
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
	if body := recorder.Body.String(); !strings.Contains(body, "has been upgraded") {
		t.Errorf("expected helm's output as SSE events, got:\n%s", body)
	}
}

func TestAnUpdatesLogsRejectNonsenseParameters(t *testing.T) {
	h := newHarness(t, nil, startedUpdate(testUpdate, testUpdateJob))

	for _, query := range []string{"?limit=0", "?limit=lots", "?since=yesterday"} {
		recorder := h.do(t, http.MethodGet, testUpdateLogs+query, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", query, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAnUpdatesLogsWithoutATelemetryStore(t *testing.T) {
	h := newHarness(t, nil, startedUpdate(testUpdate, testUpdateJob))
	h.server.logStore = func(context.Context) (logReader, error) { return nil, errNoLogStore }
	h.handler = h.server.Handler()

	recorder := h.do(t, http.MethodGet, testUpdateLogs, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

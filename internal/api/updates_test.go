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
	"strings"
	"testing"
	"time"

	"github.com/blang/semver/v4"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/chartrepo"
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

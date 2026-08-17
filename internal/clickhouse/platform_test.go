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

package clickhouse

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The unrouted bucket is counted beside the total rather than filtered out of
// it: a platform request rate that quietly dropped the hosts it could not
// attribute would disagree with what the edge actually served.
func TestPlatformRequestsCountsTheUnroutedBucket(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"requests":"3600","errors":"36","p50":"10","p95":"42.5","p99":"90","unrouted":"120"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	platform, err := store.client(t).PlatformRequests(context.Background(), PlatformRequestsQuery{
		Since: until.Add(-time.Hour),
		Until: until,
	})
	if err != nil {
		t.Fatalf("PlatformRequests: %v", err)
	}
	if platform.Requests != 3600 || platform.Unrouted != 120 {
		t.Errorf("unexpected answer: %+v", platform)
	}
	if platform.RequestsPerSecond != 1 || platform.ErrorRate != 0.01 {
		t.Errorf("the rates should be derived from the counts: %+v", platform)
	}
	if !strings.Contains(store.query, "countMergeIf(r.requests, r.project = '')") {
		t.Errorf("the unrouted bucket is the rows attributed to no project:\n%s", store.query)
	}
	// The platform's own question is the one read in this package that does
	// not name a project.
	if strings.Contains(store.query, "r.project = {project:String}") {
		t.Errorf("a platform read is not scoped to a project:\n%s", store.query)
	}
}

func TestEdgeBreakdownGroupsOnWhatWasAskedFor(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"key":"shop.example.com","project":"shop","environment":"production",` +
		`"requests":"3600","errors":"36","p50":"10","p95":"42.5","p99":"90"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	entries, err := store.client(t).EdgeBreakdown(context.Background(), EdgeBreakdownQuery{
		Since:       until.Add(-time.Hour),
		Until:       until,
		By:          EdgeByHost,
		SortBy:      RouteSortErrorRate,
		MinRequests: 100,
	})
	if err != nil {
		t.Fatalf("EdgeBreakdown: %v", err)
	}
	if !strings.Contains(store.query, "r.host AS key") {
		t.Errorf("expected the hosts to be the key:\n%s", store.query)
	}
	// The floor and the order both read the aggregate, never the String alias
	// it is selected under.
	if !strings.Contains(store.query, "HAVING "+rollupRequests+" >= {minRequests:UInt64}") {
		t.Errorf("expected the quiet rows to be dropped by the count:\n%s", store.query)
	}
	if !strings.Contains(store.query, "ORDER BY "+requestSortExpressions[RouteSortErrorRate]+" DESC") {
		t.Errorf("expected the worst error rates first:\n%s", store.query)
	}
	if got := store.params.Get("param_minRequests"); got != "100" {
		t.Errorf("the floor should travel as a parameter, got %q", got)
	}

	if len(entries) != 1 {
		t.Fatalf("want one entry, got %d", len(entries))
	}
	if entries[0].Key != "shop.example.com" || entries[0].Project != testProject {
		t.Errorf("an entry names where it belongs: %+v", entries[0])
	}
}

// The grouping and the sort both reach the statement as written, so both are
// resolved through a fixed map and an unknown one is refused.
func TestEdgeBreakdownRefusesAGroupingItDoesNotKnow(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).EdgeBreakdown(context.Background(), EdgeBreakdownQuery{
		By: "path'; DROP TABLE http_requests_1m; --",
	})
	if err == nil {
		t.Fatal("an unknown grouping should be refused")
	}
	if !strings.Contains(err.Error(), EdgeByRoute) {
		t.Errorf("the error should name the groupings that exist, got %q", err)
	}
	if len(store.queries) != 0 {
		t.Errorf("expected nothing to reach the store, got:\n%s", store.transcript())
	}
}

// The unrouted bucket is read off the raw table, because the question is about
// a handful of hosts over hours and because the raw row is where the path is.
func TestUnroutedHostsReadTheRawTable(t *testing.T) {
	store := newFakeLogStore(t)
	first := time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC)
	last := time.Date(2026, 8, 16, 11, 59, 0, 0, time.UTC)
	store.rows = `{"host":"old.example.com","requests":"3600","first":"` + stamp(first) +
		`","last":"` + stamp(last) + `"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	hosts, err := store.client(t).UnroutedHosts(context.Background(), PlatformRequestsQuery{
		Since: until.Add(-time.Hour),
		Until: until,
	})
	if err != nil {
		t.Fatalf("UnroutedHosts: %v", err)
	}
	if !strings.Contains(store.query, "FROM "+qualified(RequestsTable)) {
		t.Errorf("the unrouted bucket is a raw-row question:\n%s", store.query)
	}
	if !strings.Contains(store.query, "WHERE project = ''") {
		t.Errorf("the unrouted bucket is the rows the follower could not attribute:\n%s", store.query)
	}
	if strings.Contains(store.query, "ORDER BY requests") {
		t.Errorf("ordering on the String alias sorts 9 above 1000:\n%s", store.query)
	}

	if len(hosts) != 1 {
		t.Fatalf("want one host, got %d", len(hosts))
	}
	if hosts[0].Host != "old.example.com" || hosts[0].Requests != 3600 {
		t.Errorf("unexpected host: %+v", hosts[0])
	}
	// A host seen once an hour ago is noise; one seen throughout is a name
	// that was published and is not any more.
	if !hosts[0].FirstSeen.Equal(first) || !hosts[0].LastSeen.Equal(last) {
		t.Errorf("the window a host was seen over is what separates the two: %+v", hosts[0])
	}
}

// The overview's per-project numbers move to the requests because the flows
// answer misattributes a protected preview to the gate and an idling
// environment to the interceptor. The unrouted bucket is not a project and
// does not belong in a list of them.
func TestProjectTrafficExcludesTheUnroutedBucket(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"project":"shop","requests":"3600","errors":"36","p50":"10","p95":"42.5","p99":"90"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	traffic, err := store.client(t).ProjectTraffic(context.Background(), ProjectTrafficQuery{
		Since: until.Add(-24 * time.Hour),
		Until: until,
	})
	if err != nil {
		t.Fatalf("ProjectTraffic: %v", err)
	}
	if !strings.Contains(store.query, "r.project != ''") {
		t.Errorf("the unrouted bucket is not a project:\n%s", store.query)
	}
	// Hourly numbers over day-scale windows are the hour rollup's, and both
	// rollups hold the same states.
	if !strings.Contains(store.query, qualified(RequestsHourTable)) {
		t.Errorf("expected the hour rollup to answer:\n%s", store.query)
	}
	if len(traffic) != 1 || traffic[0].Project != testProject || traffic[0].Requests != 3600 {
		t.Fatalf("unexpected traffic: %+v", traffic)
	}
	if traffic[0].RequestsPerHour != nil {
		t.Errorf("the sparkline costs a second aggregate and was not asked for: %+v", traffic[0])
	}
}

// The sparkline is a second statement because the percentiles beside it are
// merged over the whole window: a mean of hourly p95s is not a p95.
func TestProjectTrafficFillsTheSparklineFromASecondAggregate(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	store.answer = func(query string) string {
		if strings.Contains(query, "AS hour") {
			return `{"project":"shop","hour":"` + stamp(start) + `","requests":"600"}` + "\n" +
				`{"project":"shop","hour":"` + stamp(start.Add(2*time.Hour)) + `","requests":"1200"}`
		}
		return `{"project":"shop","requests":"1800","errors":"18","p50":"10","p95":"42.5","p99":"90"}`
	}

	traffic, err := store.client(t).ProjectTraffic(context.Background(), ProjectTrafficQuery{
		Since:     start,
		Until:     start.Add(3 * time.Hour),
		Sparkline: true,
	})
	if err != nil {
		t.Fatalf("ProjectTraffic: %v", err)
	}
	if len(traffic) != 1 {
		t.Fatalf("want one project, got %d", len(traffic))
	}
	hours := traffic[0].RequestsPerHour
	if len(hours) != 4 {
		t.Fatalf("want four hourly points, got %d", len(hours))
	}
	if hours[0] != 600 || hours[1] != 0 || hours[2] != 1200 {
		t.Errorf("the quiet hour is silence, not an omission: %v", hours)
	}
	// The totals are the merged window and not the sum of the sparkline.
	if traffic[0].Requests != 1800 || traffic[0].P95Ms != 42.5 {
		t.Errorf("unexpected totals: %+v", traffic[0])
	}
}

// A node whose collector is dead — or was never admitted, which is the
// PodSecurity failure that leaves a DaemonSet with no pods at all — looks
// clean on every screen that counts what is running. This is the one that
// looks broken.
func TestTelemetryFreshnessReadsBothTheLogsAndTheMetrics(t *testing.T) {
	store := newFakeLogStore(t)
	seen := time.Date(2026, 8, 16, 11, 59, 0, 0, time.UTC)
	store.rows = `{"node":"node-1","lastSeen":"` + stamp(seen) + `"}`

	freshness, err := store.client(t).TelemetryFreshness(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("TelemetryFreshness: %v", err)
	}
	for _, table := range []string{LogsTable, MetricsGaugeTable} {
		if !strings.Contains(store.query, qualified(table)) {
			t.Errorf("a half-working collector is still a collector to look at; %s is missing:\n%s",
				table, store.query)
		}
	}
	if !strings.Contains(store.query, "UNION ALL") {
		t.Errorf("the two sources are one answer:\n%s", store.query)
	}
	if !strings.Contains(store.query, "node != ''") {
		t.Errorf("a row that names no node says nothing about one:\n%s", store.query)
	}

	if len(freshness) != 1 {
		t.Fatalf("want one node, got %d", len(freshness))
	}
	if freshness[0].Node != "node-1" || !freshness[0].LastSeen.Equal(seen) {
		t.Errorf("unexpected freshness: %+v", freshness[0])
	}
}

func TestAPlatformReadRefusesABackwardsWindow(t *testing.T) {
	store := newFakeLogStore(t)
	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	query := PlatformRequestsQuery{Since: until, Until: until.Add(-time.Hour)}

	if _, err := store.client(t).PlatformRequests(context.Background(), query); err == nil {
		t.Error("a backwards window should be refused before it reaches the store")
	}
	if _, err := store.client(t).UnroutedHosts(context.Background(), query); err == nil {
		t.Error("a backwards window should be refused before it reaches the store")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected nothing to reach the store, got:\n%s", store.transcript())
	}
}

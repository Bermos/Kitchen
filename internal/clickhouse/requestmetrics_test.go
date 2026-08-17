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

// A rollup row as ClickHouse renders one: everything cast to String in the
// statement, which is what gives the readers one decoding path.
func rollupRow(bucket time.Time, requests, errors string) string {
	return `{"slot":"` + bucket.Format("2006-01-02 15:04:05") + `","bucket":"` + stamp(bucket) +
		`","requests":"` + requests + `","errors":"` + errors +
		`","p50":"10","p95":"42.5","p99":"90"}`
}

// The state columns are called `requests` and `duration`, and so are the values
// selected out of them. ClickHouse resolves a bare name against the SELECT
// list's own aliases before the table's columns, so an unqualified
// `countMerge(requests)` would be a merge of the merge — the same shadowing
// that makes the metrics rollup's view refuse to be created.
func TestTheGoldenSignalsReadTheStateColumnsThroughTheAlias(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}

	for _, want := range []string{
		"countMerge(r.requests)",
		"countMergeIf(r.requests, r.status >= 500)",
		"quantilesTDigestMerge(0.5, 0.95, 0.99)(r.duration)",
		"FROM " + qualified(RequestsMinuteTable) + " AS r",
	} {
		if !strings.Contains(store.query, want) {
			t.Errorf("expected %q in:\n%s", want, store.query)
		}
	}
	for _, unwanted := range []string{"countMerge(requests)", "(duration)", "AND bucket >="} {
		if strings.Contains(store.query, unwanted) {
			t.Errorf("an unqualified %q shadows the column it reads:\n%s", unwanted, store.query)
		}
	}
	// A merge over no rows answers nan, which is not a JSON number.
	if !strings.Contains(store.query, "ifNotFinite(") {
		t.Errorf("the percentiles should be guarded against an empty window:\n%s", store.query)
	}
}

// The two rollups hold the same states, so the choice is about what a read
// costs and never about what it says.
func TestAWideWindowIsAnsweredFromTheHourRollup(t *testing.T) {
	store := newFakeLogStore(t)
	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	summary, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
		Since:   until.Add(-30 * 24 * time.Hour),
		Until:   until,
	})
	if err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if summary.Rollup != RequestRollupHour {
		t.Errorf("a month-wide summary should read the hour rollup, got %q", summary.Rollup)
	}
	if !strings.Contains(store.query, qualified(RequestsHourTable)) {
		t.Errorf("expected the hour rollup to answer:\n%s", store.query)
	}

	store = newFakeLogStore(t)
	summary, err = store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
		Since:   until.Add(-time.Hour),
		Until:   until,
	})
	if err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if summary.Rollup != RequestRollupMinute {
		t.Errorf("an hour-wide summary should read the minute rollup, got %q", summary.Rollup)
	}
}

// A rollup bucket is indivisible, so a window that starts inside one either
// takes the whole bucket or misses it. Missing it is not a rounding error: an
// hour-rollup read of a window that began at 12:34 would compare against a
// bucket stamped 12:00, match nothing, and report traffic that plainly
// happened as none.
func TestARollupReadSnapsItsWindowBackToTheBucket(t *testing.T) {
	store := newFakeLogStore(t)
	until := time.Date(2026, 8, 16, 12, 34, 56, 0, time.UTC)

	summary, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
		Since:   until.Add(-72 * time.Hour),
		Until:   until,
	})
	if err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if summary.Rollup != RequestRollupHour {
		t.Fatalf("a three-day window should read the hour rollup, got %q", summary.Rollup)
	}

	since, err := time.Parse(time.RFC3339Nano, store.params.Get("param_since"))
	if err != nil {
		t.Fatalf("reading the window's start: %v", err)
	}
	if !since.Equal(since.Truncate(time.Hour)) {
		t.Errorf("the hour rollup should be asked from the top of an hour, got %s", since)
	}
	// And the window it reports is the one it answered, so the rate below it
	// is per something true.
	if !summary.Since.Equal(since) {
		t.Errorf("the reported window %s is not the one asked for, %s", summary.Since, since)
	}
}

func TestRequestSummaryDerivesItsRatesFromTheCounts(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"requests":"3600","errors":"36","p50":"10","p95":"42.5","p99":"90"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	summary, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project:     testProject,
		Environment: testEnvironment,
		Since:       until.Add(-time.Hour),
		Until:       until,
	})
	if err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if summary.Requests != 3600 || summary.Errors != 36 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if summary.RequestsPerSecond != 1 {
		t.Errorf("3600 requests over an hour is one per second, got %v", summary.RequestsPerSecond)
	}
	if summary.ErrorRate != 0.01 {
		t.Errorf("36 of 3600 is a hundredth, got %v", summary.ErrorRate)
	}
	if summary.P95Ms != 42.5 || summary.P99Ms != 90 {
		t.Errorf("the percentiles did not survive: %+v", summary)
	}
}

// An environment that has served nothing still answers, with zeroes: an
// aggregate over no rows is one row of nothing, and the empty screen is drawn
// from it.
func TestRequestSummaryAnswersAnEmptyWindowWithZeroes(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = ""

	summary, err := store.client(t).RequestSummary(context.Background(), RequestQuery{Project: testProject})
	if err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if summary.Requests != 0 || summary.ErrorRate != 0 || summary.P95Ms != 0 {
		t.Errorf("an empty window is zeroes, got %+v", summary)
	}
}

func TestARequestReadMustNameAProject(t *testing.T) {
	store := newFakeLogStore(t)
	client := store.client(t)
	ctx := context.Background()

	if _, err := client.RequestSummary(ctx, RequestQuery{}); err == nil {
		t.Error("a summary with no project should be refused")
	}
	if _, err := client.RequestSeries(ctx, RequestSeriesQuery{}); err == nil {
		t.Error("a series with no project should be refused")
	}
	if _, err := client.RequestRoutes(ctx, RequestRoutesQuery{}); err == nil {
		t.Error("a route table with no project should be refused")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected nothing to reach the store, got:\n%s", store.transcript())
	}
}

// The series carries the empty buckets, because an environment that stopped
// serving is the most interesting shape a traffic chart has.
func TestARequestSeriesFillsEveryBucketInTheWindow(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	store.rows = strings.Join([]string{
		rollupRow(start, "600", "6"),
		rollupRow(start.Add(2*time.Minute), "120", "60"),
	}, "\n")

	series, err := store.client(t).RequestSeries(context.Background(), RequestSeriesQuery{
		RequestQuery: RequestQuery{
			Project:     testProject,
			Environment: testEnvironment,
			Since:       start,
			Until:       start.Add(6 * time.Minute),
		},
		Buckets: 6,
	})
	if err != nil {
		t.Fatalf("RequestSeries: %v", err)
	}

	if series.BucketSeconds != 60 {
		t.Fatalf("a six-minute window over six buckets is a minute each, got %ds", series.BucketSeconds)
	}
	if series.Rollup != RequestRollupMinute {
		t.Fatalf("a minute-wide bucket is the minute rollup's, got %q", series.Rollup)
	}
	if len(series.Points) != 7 {
		t.Fatalf("want seven points (both boundaries included), got %d", len(series.Points))
	}
	if series.Points[0].Requests != 600 || series.Points[0].RequestsPerSecond != 10 {
		t.Errorf("600 requests in a minute is ten a second: %+v", series.Points[0])
	}
	if series.Points[1] != (RequestPoint{Start: start.Add(time.Minute)}) {
		t.Errorf("the quiet bucket should be zero, got %+v", series.Points[1])
	}
	if series.Points[2].ErrorRate != 0.5 {
		t.Errorf("60 of 120 is half: %+v", series.Points[2])
	}
}

// A bucket an hour wide or wider is the hour rollup's by definition; anything
// finer than a minute is finer than the finest rollup there is.
func TestARequestSeriesPicksItsRollupFromTheBucketWidth(t *testing.T) {
	store := newFakeLogStore(t)
	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	series, err := store.client(t).RequestSeries(context.Background(), RequestSeriesQuery{
		RequestQuery: RequestQuery{
			Project: testProject,
			Since:   until.Add(-30 * 24 * time.Hour),
			Until:   until,
		},
		Buckets: 60,
	})
	if err != nil {
		t.Fatalf("RequestSeries: %v", err)
	}
	if series.Rollup != RequestRollupHour || !strings.Contains(store.query, qualified(RequestsHourTable)) {
		t.Errorf("a month over 60 points is half-day buckets, which the hour rollup answers: %q\n%s",
			series.Rollup, store.query)
	}

	store = newFakeLogStore(t)
	series, err = store.client(t).RequestSeries(context.Background(), RequestSeriesQuery{
		RequestQuery: RequestQuery{
			Project: testProject,
			Since:   until.Add(-time.Minute),
			Until:   until,
		},
		Buckets: 480,
	})
	if err != nil {
		t.Fatalf("RequestSeries: %v", err)
	}
	if series.BucketSeconds != 60 {
		t.Errorf("nothing is finer than the minute rollup, got %ds buckets", series.BucketSeconds)
	}
}

// The counts are selected as Strings, and a String orders lexicographically:
// "9" above "1000". The route table's limit decides which routes a person ever
// sees, so ordering on the rendering rather than the number would quietly keep
// the quietest.
func TestRequestRoutesOrdersOnTheAggregateNotItsRendering(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestRoutes(context.Background(), RequestRoutesQuery{
		RequestQuery: RequestQuery{Project: testProject},
	}); err != nil {
		t.Fatalf("RequestRoutes: %v", err)
	}
	if strings.Contains(store.query, "ORDER BY requests") {
		t.Errorf("ordering on the String alias sorts 9 above 1000:\n%s", store.query)
	}
	if !strings.Contains(store.query, "ORDER BY "+rollupRequests+" DESC") {
		t.Errorf("expected the busiest routes to be kept:\n%s", store.query)
	}
	if !strings.Contains(store.query, "GROUP BY route") {
		t.Errorf("the route table groups by the template:\n%s", store.query)
	}
}

func TestRequestRoutesSortsOnWhatWasAskedFor(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestRoutes(context.Background(), RequestRoutesQuery{
		RequestQuery: RequestQuery{Project: testProject},
		SortBy:       RouteSortLatency,
	}); err != nil {
		t.Fatalf("RequestRoutes: %v", err)
	}
	if !strings.Contains(store.query, "ORDER BY "+rollupQuantile(2)+" DESC") {
		t.Errorf("expected the slowest routes first:\n%s", store.query)
	}

	// An unknown sort is the caller's mistake and is named as such, rather
	// than being quietly answered in some other order.
	_, err := store.client(t).RequestRoutes(context.Background(), RequestRoutesQuery{
		RequestQuery: RequestQuery{Project: testProject},
		SortBy:       "requests DESC; DROP TABLE http_requests",
	})
	if err == nil {
		t.Fatal("an unknown sort should be refused")
	}
	if !strings.Contains(err.Error(), RouteSortErrorRate) {
		t.Errorf("the error should name the sorts that exist, got %q", err)
	}
}

func TestRequestRoutesReadsARowIntoARoute(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"route":"/works/:id","requests":"3600","errors":"36",` +
		`"p50":"10","p95":"42.5","p99":"90"}`

	until := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	routes, err := store.client(t).RequestRoutes(context.Background(), RequestRoutesQuery{
		RequestQuery: RequestQuery{
			Project:     testProject,
			Environment: testEnvironment,
			Since:       until.Add(-time.Hour),
			Until:       until,
		},
	})
	if err != nil {
		t.Fatalf("RequestRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("want one route, got %d", len(routes))
	}
	route := routes[0]
	if route.Route != "/works/:id" || route.Requests != 3600 || route.Errors != 36 {
		t.Errorf("unexpected route: %+v", route)
	}
	if route.RequestsPerSecond != 1 || route.ErrorRate != 0.01 {
		t.Errorf("the rates should be derived from the counts: %+v", route)
	}
}

// Narrowing the charts to one route is what clicking a row of the route table
// does, and the value comes back out of a URL.
func TestARequestReadNarrowsToARouteByParameter(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
		Route:   "/works/:id'; DROP TABLE http_requests_1m; --",
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the route reached the query text:\n%s", store.query)
	}
	if !strings.Contains(store.query, "r.route = {route:String}") {
		t.Errorf("expected the route to be matched as a parameter:\n%s", store.query)
	}
}

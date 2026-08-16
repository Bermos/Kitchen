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

// The environment page's whole reason for existing: an hour of samples asked
// for at a minute's resolution is bucketed off the raw table, and the answer
// carries the empty buckets so that silence reads as silence.
func TestAResourceSeriesFillsEveryBucketInTheWindow(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	store.rows = strings.Join([]string{
		`{"bucket":"` + stamp(start) + `","cpu":"0.25","cpuPeak":"0.4","memory":"200000000",` +
			`"memoryPeak":"220000000","replicas":"2","restarts":"0","oomKills":"0",` +
			`"cpuLimit":"0.5","memoryLimit":"536870912"}`,
		`{"bucket":"` + stamp(start.Add(2*time.Minute)) + `","cpu":"0.3","cpuPeak":"0.6","memory":"210000000",` +
			`"memoryPeak":"250000000","replicas":"1","restarts":"1","oomKills":"1",` +
			`"cpuLimit":"0.5","memoryLimit":"536870912"}`,
	}, "\n")

	series, err := store.client(t).ResourceSeries(context.Background(), ResourceSeriesQuery{
		Project:     "shop",
		Environment: "production",
		Since:       start,
		Until:       start.Add(6 * time.Minute),
		Buckets:     6,
	})
	if err != nil {
		t.Fatalf("ResourceSeries: %v", err)
	}

	if series.BucketSeconds != 60 {
		t.Fatalf("a six-minute window over six buckets is a minute each, got %ds", series.BucketSeconds)
	}
	if len(series.Points) != 7 {
		t.Fatalf("want seven points (both boundaries included), got %d", len(series.Points))
	}
	if series.Points[0].Replicas != 2 || series.Points[0].CPUCores != 0.25 {
		t.Fatalf("the first bucket lost its numbers: %+v", series.Points[0])
	}
	// The bucket between the two that reported is silence, not an omission.
	if series.Points[1] != (ResourcePoint{Start: start.Add(time.Minute)}) {
		t.Fatalf("the quiet bucket should be zero, got %+v", series.Points[1])
	}
	if series.Points[2].Restarts != 1 || series.Points[2].OOMKills != 1 {
		t.Fatalf("the restart and its cause should land in their own bucket: %+v", series.Points[2])
	}
	if series.Restarts != 1 || series.OOMKills != 1 {
		t.Fatalf("the window totals should add the series up, got %d restarts and %d kills",
			series.Restarts, series.OOMKills)
	}
	if series.MemoryLimitBytes != 536870912 {
		t.Fatalf("the limits should come off the newest bucket that reported, got %d", series.MemoryLimitBytes)
	}
	if series.Rollup {
		t.Fatal("a minute of resolution is finer than the rollup and has to come off the raw samples")
	}
	if !strings.Contains(store.query, "`kitchen`.`metrics`") {
		t.Fatalf("the fine window should have read the raw table:\n%s", store.query)
	}
}

// A window wide enough to be answered at five minutes or coarser is answered
// from the rollup, which is the only reason the rollup exists.
func TestAWideWindowReadsTheRollup(t *testing.T) {
	store := newFakeLogStore(t)
	until := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	series, err := store.client(t).ResourceSeries(context.Background(), ResourceSeriesQuery{
		Project:     "shop",
		Environment: "production",
		Since:       until.Add(-24 * time.Hour),
		Until:       until,
		Buckets:     60,
	})
	if err != nil {
		t.Fatalf("ResourceSeries: %v", err)
	}
	if !series.Rollup {
		t.Fatalf("a day at sixty points is %ds a bucket, which the rollup covers", series.BucketSeconds)
	}
	if !strings.Contains(store.query, MetricsRollupTable) {
		t.Fatalf("the wide window should have read the rollup:\n%s", store.query)
	}
	// Aggregate states are merged, never read: selecting one directly returns
	// an opaque blob rather than a number.
	for _, merge := range []string{"avgMerge", "maxMerge", "sumMerge"} {
		if !strings.Contains(store.query, merge) {
			t.Fatalf("the rollup read is missing %s:\n%s", merge, store.query)
		}
	}
}

// Everything the caller supplied travels as a parameter. A project called
// `'; DROP` matches nothing; it is not a statement.
func TestAResourceSeriesPassesItsScopeAsParameters(t *testing.T) {
	store := newFakeLogStore(t)
	project := "shop'; DROP TABLE metrics --"

	_, err := store.client(t).ResourceSeries(context.Background(), ResourceSeriesQuery{
		Project:     project,
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("ResourceSeries: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("a project name reached the statement text:\n%s", store.query)
	}
	if got := store.params.Get("param_project"); got != project {
		t.Fatalf("the project should have travelled as a parameter, got %q", got)
	}
}

// The two scoping fields are not optional: this answers for one workload, and
// an unscoped version would read the whole platform's samples to draw one
// environment's chart.
func TestAResourceSeriesRefusesAnUnscopedQuery(t *testing.T) {
	store := newFakeLogStore(t)
	for _, query := range []ResourceSeriesQuery{
		{Environment: "production"},
		{Project: "shop"},
		{},
	} {
		if _, err := store.client(t).ResourceSeries(context.Background(), query); err == nil {
			t.Fatalf("ResourceSeries(%+v) should have been refused", query)
		}
	}
}

func TestAResourceWindowMustEndAfterItStarts(t *testing.T) {
	store := newFakeLogStore(t)
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)

	_, err := store.client(t).ResourceSeries(context.Background(), ResourceSeriesQuery{
		Project:     "shop",
		Environment: "production",
		Since:       start,
		Until:       start.Add(-time.Hour),
	})
	if err == nil {
		t.Fatal("a backwards window should have been refused")
	}
}

// Samples are written as JSONEachRow, so a Go bool has to arrive as the UInt8
// the column is.
func TestInsertingSamplesWritesEveryColumn(t *testing.T) {
	store := newFakeLogStore(t)

	err := store.client(t).InsertResourceSamples(context.Background(), []ResourceSample{{
		Timestamp:   time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC),
		Project:     "shop",
		Environment: "production",
		Pod:         "production-abc-1",
		Container:   "app",
		CPUCores:    0.25,
		MemoryBytes: 200000000,
		Restarts:    3,
		Restarted:   1,
		OOMKilled:   true,
	}})
	if err != nil {
		t.Fatalf("InsertResourceSamples: %v", err)
	}
	for _, fragment := range []string{
		"INSERT INTO `kitchen`.`metrics` FORMAT JSONEachRow",
		`"oomKilled":1`,
		`"restarted":1`,
		`"restarts":3`,
		`"timestamp":"2026-08-16 10:00:00.000"`,
	} {
		if !strings.Contains(store.query, fragment) {
			t.Fatalf("the insert should have carried %s:\n%s", fragment, store.query)
		}
	}
}

// An empty batch is not an empty INSERT.
func TestInsertingNoSamplesAsksNothing(t *testing.T) {
	store := newFakeLogStore(t)
	if err := store.client(t).InsertResourceSamples(context.Background(), nil); err != nil {
		t.Fatalf("InsertResourceSamples: %v", err)
	}
	if store.query != "" {
		t.Fatalf("nothing should have been sent, got %q", store.query)
	}
}

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
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTheHistogramBucketsTheWindow(t *testing.T) {
	store := newFakeLogStore(t)
	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	store.rows = strings.Join([]string{
		`{"bucket":"` + stamp(since) + `","hits":"10","errors":"2","warnings":"1"}`,
		`{"bucket":"` + stamp(since.Add(2*time.Minute)) + `","hits":"5","errors":"0","warnings":"0"}`,
	}, "\n")

	histogram, err := store.client(t).LogHistogram(context.Background(), LogHistogramQuery{
		LogSelection: LogSelection{Query: "level:error", Since: since, Until: until,
			Scope: LogScope{Platform: true}},
		Buckets: 60,
	})
	if err != nil {
		t.Fatalf("LogHistogram: %v", err)
	}
	if histogram.BucketSeconds != 60 {
		t.Fatalf("an hour over 60 bars is a minute a bar, got %d", histogram.BucketSeconds)
	}
	if len(histogram.Buckets) != 61 {
		t.Fatalf("want the window's buckets including the empty ones, got %d", len(histogram.Buckets))
	}
	if histogram.Buckets[0].Count != 10 || histogram.Buckets[0].Errors != 2 {
		t.Fatalf("the first bucket did not land: %+v", histogram.Buckets[0])
	}
	// A gap is information, so an unreported bucket is present and zero.
	if histogram.Buckets[1].Count != 0 {
		t.Fatalf("a bucket nothing was reported for should be zero: %+v", histogram.Buckets[1])
	}
	if histogram.Buckets[2].Count != 5 {
		t.Fatalf("the second reported bucket did not land: %+v", histogram.Buckets[2])
	}
	if histogram.Total != 15 {
		t.Fatalf("want 15 lines in the window, got %d", histogram.Total)
	}

	// The query language compiled into the predicate, and the value it matched
	// travelled as a parameter.
	if !strings.Contains(store.query, logLevelColumn+" = {q0:String}") {
		t.Fatalf("the query should compile into the histogram's predicate:\n%s", store.query)
	}
	if got := store.params.Get("param_q0"); got != levelError {
		t.Fatalf("the value should travel as a parameter, got %q", got)
	}
	if got := store.params.Get("readonly"); got != "2" {
		t.Fatalf("an analytic carrying caller query text must run read-only, got %q", got)
	}
}

// The ladder exists so that panning the window does not restripe the chart at
// some arbitrary width, and so that a bucket boundary is a round number.
func TestHistogramBucketsComeOffTheLadder(t *testing.T) {
	for _, test := range []struct {
		span    time.Duration
		buckets int
		want    int
	}{
		{time.Hour, 60, 60},
		{15 * time.Minute, 60, 15},
		{24 * time.Hour, 60, 1800},
		{7 * 24 * time.Hour, 60, 10800},
		{time.Minute, 60, 1},
	} {
		if got := bucketSeconds(test.span, test.buckets); got != test.want {
			t.Errorf("%s over %d bars should bucket at %ds, got %ds",
				test.span, test.buckets, test.want, got)
		}
	}
}

// An unbounded selection has no span to bucket, so the store is asked what the
// span actually is rather than a day being assumed.
func TestAnUnboundedHistogramAsksTheStoreForItsWindow(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"first":"0","last":"0"}`

	histogram, err := store.client(t).LogHistogram(context.Background(),
		LogHistogramQuery{LogSelection: LogSelection{Scope: LogScope{Platform: true}}})
	if err != nil {
		t.Fatalf("LogHistogram: %v", err)
	}
	if len(histogram.Buckets) != 0 {
		t.Fatalf("an empty store should draw nothing, not a flat line: %+v", histogram)
	}
	if !strings.Contains(store.query, "min(Timestamp)") {
		t.Fatalf("the span should be read from the store:\n%s", store.query)
	}
}

func TestFacetsAreCountedOverTheWindow(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = strings.Join([]string{
		`{"facet":"level","value":"info","hits":"90"}`,
		`{"facet":"level","value":"error","hits":"120"}`,
		`{"facet":"stream","value":"stderr","hits":"3"}`,
	}, "\n")

	facets, err := store.client(t).LogFacets(context.Background(), LogFacetQuery{
		LogSelection: LogSelection{Query: "service:shop", Scope: LogScope{Platform: true}},
		Fields:       []string{"level", "stream"},
	})
	if err != nil {
		t.Fatalf("LogFacets: %v", err)
	}
	if len(facets) != 2 || facets[0].Field != "level" || facets[1].Field != "stream" {
		t.Fatalf("facets should come back in the order they were asked for: %+v", facets)
	}
	// UNION ALL promises no order across its branches, so each facet is
	// re-sorted: commonest first.
	if facets[0].Values[0].Value != levelError || facets[0].Values[0].Count != 120 {
		t.Fatalf("the commonest value should come first: %+v", facets[0].Values)
	}
	// One round trip, however many facets are shown.
	if strings.Count(store.query, "UNION ALL") != 1 {
		t.Fatalf("two facets should be one query:\n%s", store.query)
	}
}

// A window where nothing carries a level still has a level facet, and it is an
// empty list rather than a null: the sidebar iterates every facet it is given.
func TestAFacetNothingHoldsIsEmptyRatherThanNull(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"facet":"stream","value":"stderr","hits":"2"}`

	facets, err := store.client(t).LogFacets(context.Background(), LogFacetQuery{
		LogSelection: LogSelection{Scope: LogScope{Platform: true}},
		Fields:       []string{"level", "stream"},
	})
	if err != nil {
		t.Fatalf("LogFacets: %v", err)
	}
	if len(facets) != 2 || facets[0].Field != "level" {
		t.Fatalf("an empty facet should still be asked and answered: %+v", facets)
	}
	if facets[0].Values == nil {
		t.Fatal("an empty facet's values should be an empty list, not nil")
	}
	body, err := json.Marshal(facets[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"values":[]`) {
		t.Fatalf("an empty facet should serialise as an empty list: %s", body)
	}
}

// A facet on something that is not a column is a log attribute, resolved the
// same way the query language resolves it — so a user can facet on what they
// can query on.
func TestAFacetOnAnAttributeResolvesLikeAQuery(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"facet":"http.status","value":"500","hits":"7"}`

	if _, err := store.client(t).LogFacets(context.Background(), LogFacetQuery{
		LogSelection: LogSelection{Scope: LogScope{Platform: true}},
		Fields:       []string{"http.status"},
	}); err != nil {
		t.Fatalf("LogFacets: %v", err)
	}
	if !strings.Contains(store.query, "LogAttributes[{facet0:String}]") {
		t.Fatalf("a facet on an attribute should read the map:\n%s", store.query)
	}
	if got := store.params.Get("param_facet0"); got != "http.status" {
		t.Fatalf("the field name should travel as a parameter, got %q", got)
	}
}

func TestPatternsCollapseTheVariableParts(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"pattern":"GET /works?page=<n> <n>","hits":"14021","level":"info",` +
		`"sample":"GET /works?page=7 200","first":"1786000000","last":"1786003600"}`

	patterns, err := store.client(t).LogPatterns(context.Background(), LogPatternQuery{
		LogSelection: LogSelection{Query: "service:shop", Scope: LogScope{Platform: true}},
	})
	if err != nil {
		t.Fatalf("LogPatterns: %v", err)
	}
	if len(patterns) != 1 || patterns[0].Count != 14021 {
		t.Fatalf("unexpected patterns: %+v", patterns)
	}
	if patterns[0].Sample != "GET /works?page=7 200" {
		t.Fatalf("a template should keep a line it stands for: %+v", patterns[0])
	}
	if patterns[0].FirstSeen.IsZero() || !patterns[0].LastSeen.After(patterns[0].FirstSeen) {
		t.Fatalf("a template should carry when it was seen: %+v", patterns[0])
	}

	// Normalising is a regular expression per line, so it runs over a bounded
	// slice of the newest matching lines rather than the whole window.
	if got := store.params.Get("param_scan"); got != "20000" {
		t.Fatalf("the scan should be bounded, got %q", got)
	}
	for _, normaliser := range patternNormalisers {
		if !strings.Contains(store.query, normaliser.replacement) {
			t.Fatalf("every normaliser should be applied, %s is missing:\n%s",
				normaliser.replacement, store.query)
		}
	}
}

func TestAnalyticsBoundWhatTheyReturn(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).LogPatterns(context.Background(), LogPatternQuery{
		LogSelection: LogSelection{Scope: LogScope{Platform: true}},
		Limit:        100000, Scan: 100000000,
	}); err != nil {
		t.Fatalf("LogPatterns: %v", err)
	}
	if got := store.params.Get("param_limit"); got != "200" {
		t.Fatalf("the pattern count should be capped at %d, got %q", MaxPatternRows, got)
	}
	if got := store.params.Get("param_scan"); got != "200000" {
		t.Fatalf("the scan should be capped at %d, got %q", MaxPatternScan, got)
	}

	if _, err := store.client(t).LogFacets(context.Background(),
		LogFacetQuery{LogSelection: LogSelection{Scope: LogScope{Platform: true}}, Limit: 5000}); err != nil {
		t.Fatalf("LogFacets: %v", err)
	}
	if got := store.params.Get("param_facetLimit"); got != "20" {
		t.Fatalf("a facet should be capped at %d values, got %q", MaxFacetValues, got)
	}
}

// A refused query never reaches the store: the parser answers first, and its
// error is the caller's to fix.
func TestAnalyticsRefuseAnUnparseableQuery(t *testing.T) {
	store := newFakeLogStore(t)
	selection := LogSelection{Query: "(level:error", Scope: LogScope{Platform: true}}

	if _, err := store.client(t).LogHistogram(context.Background(),
		LogHistogramQuery{LogSelection: selection}); err == nil {
		t.Fatal("the histogram should refuse an unparseable query")
	}
	if _, err := store.client(t).LogFacets(context.Background(),
		LogFacetQuery{LogSelection: selection}); err == nil {
		t.Fatal("the facets should refuse an unparseable query")
	}
	if _, err := store.client(t).LogPatterns(context.Background(),
		LogPatternQuery{LogSelection: selection}); err == nil {
		t.Fatal("the patterns should refuse an unparseable query")
	}
}

// stamp renders a bucket boundary the way ClickHouse reports one.
func stamp(at time.Time) string {
	return strconv.FormatInt(at.Unix(), 10)
}

// The histogram splits the bad news out of the count, and it does so against
// the folded level: SeverityText arrives spelled however the emitter spelled
// it, and a bar that missed every `ERROR` would be the chart's whole point
// missed.
func TestTheHistogramCountsSeverityCaseInsensitively(t *testing.T) {
	store := newFakeLogStore(t)
	since := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)

	if _, err := store.client(t).LogHistogram(context.Background(), LogHistogramQuery{
		LogSelection: LogSelection{Since: since, Until: since.Add(time.Hour),
			Scope: LogScope{Platform: true}},
	}); err != nil {
		t.Fatalf("LogHistogram: %v", err)
	}
	for _, want := range []string{
		"countIf(" + logLevelColumn + " IN ('error', 'fatal'))",
		"countIf(" + logLevelColumn + " = 'warn')",
	} {
		if !strings.Contains(store.query, want) {
			t.Fatalf("the histogram is missing %q:\n%s", want, store.query)
		}
	}
	if !strings.Contains(store.query, "toStartOfInterval(Timestamp") {
		t.Fatalf("the histogram should bucket the exporter's time column:\n%s", store.query)
	}
}

// Patterns are extracted from the message column, which the exporter calls
// Body. The normalisers are built over the column rather than over the
// projection's `message` alias — ClickHouse resolves a SELECT alias before a
// column, and depending on that ordering is how this package has been bitten
// before.
func TestPatternsNormaliseTheColumnNotTheAlias(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).LogPatterns(context.Background(),
		LogPatternQuery{LogSelection: LogSelection{Scope: LogScope{Platform: true}}}); err != nil {
		t.Fatalf("LogPatterns: %v", err)
	}
	if !strings.Contains(store.query, "replaceRegexpAll("+logMessageColumn+",") {
		t.Fatalf("the innermost normaliser should read the column:\n%s", store.query)
	}
	if !strings.Contains(store.query, logMessageColumn+" AS message") {
		t.Fatalf("the sample should still come back under Kitchen's name:\n%s", store.query)
	}
}

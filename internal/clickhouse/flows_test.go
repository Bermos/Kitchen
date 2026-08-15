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

// The traffic page answered 500 on every load because the derived protocol was
// selected `AS protocol`, which is also a column of the flows table. ClickHouse
// resolves an identifier in the SELECT list against the aliases first, so the
// `protocol` inside the p95's condition became the whole
// `if(countIf(…) > 0, …)` expression — a countIf nested in a quantileIf, which
// is the ILLEGAL_AGGREGATION the store refused with.
func TestTheDerivedProtocolDoesNotShadowTheColumn(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).TrafficEdges(context.Background(), TrafficQuery{}); err != nil {
		t.Fatalf("TrafficEdges: %v", err)
	}

	if strings.Contains(store.query, "AS protocol") {
		t.Fatalf("the derived protocol must not take the column's name:\n%s", store.query)
	}
	if !strings.Contains(store.query, "AS "+protocolAlias) {
		t.Fatalf("the derived protocol should be selected as %q:\n%s", protocolAlias, store.query)
	}
	// Both aggregates still read the column, which is the point of renaming
	// the alias rather than reshaping the aggregation.
	if !strings.Contains(store.query, "countIf(protocol = 'HTTP')") {
		t.Fatalf("the dominant-protocol test should read the column:\n%s", store.query)
	}
	if !strings.Contains(store.query, "quantileIf(0.95)(latencyMs, protocol = 'HTTP' AND latencyMs > 0)") {
		t.Fatalf("the p95 should read the column:\n%s", store.query)
	}
}

// The busiest edges are what a 500-row limit should keep. `flows` is the count
// rendered as a String, and ordering on it sorts lexicographically: "9" above
// "1000".
func TestTrafficEdgesOrdersOnTheCountNotItsRendering(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).TrafficEdges(context.Background(), TrafficQuery{}); err != nil {
		t.Fatalf("TrafficEdges: %v", err)
	}
	if strings.Contains(store.query, "ORDER BY flows") {
		t.Fatalf("ordering on the String alias sorts 9 above 1000:\n%s", store.query)
	}
	if !strings.Contains(store.query, "ORDER BY count() DESC") {
		t.Fatalf("the busiest edges should be kept:\n%s", store.query)
	}
}

func TestTrafficEdgesReadsARowIntoAnEdge(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"source":"web","sourceNamespace":"kitchen-shop",` +
		`"destination":"api","destinationNamespace":"kitchen-shop",` +
		`"edgeProtocol":"HTTP","flows":"1800","errors":"12","drops":"3","p95Ms":42.5}`

	until := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	edges, err := store.client(t).TrafficEdges(context.Background(), TrafficQuery{
		Since: until.Add(-30 * time.Minute),
		Until: until,
	})
	if err != nil {
		t.Fatalf("TrafficEdges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("want one edge, got %d", len(edges))
	}

	edge := edges[0]
	// The alias is a query-text detail; the API still answers with `protocol`.
	if edge.Protocol != "HTTP" {
		t.Errorf("protocol: want HTTP, got %q", edge.Protocol)
	}
	if edge.Flows != 1800 || edge.Errors != 12 || edge.Drops != 3 {
		t.Errorf("unexpected counts: %+v", edge)
	}
	if edge.P95Ms != 42.5 {
		t.Errorf("p95: want 42.5, got %v", edge.P95Ms)
	}
	// 1800 flows over a half-hour window is one per second.
	if edge.RPS != 1 {
		t.Errorf("rps: want 1, got %v", edge.RPS)
	}
}

func TestTrafficEdgesPassesTheNamespaceAsAParameter(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).TrafficEdges(context.Background(), TrafficQuery{
		Namespace: "kitchen-shop'; DROP TABLE flows; --",
	})
	if err != nil {
		t.Fatalf("TrafficEdges: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the namespace reached the query text:\n%s", store.query)
	}
	if got := store.params.Get("param_namespace"); got != "kitchen-shop'; DROP TABLE flows; --" {
		t.Fatalf("the namespace should travel as a parameter, got %q", got)
	}
}

func TestTrafficEdgesRefusesAWindowThatEndsBeforeItStarts(t *testing.T) {
	store := newFakeLogStore(t)

	until := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if _, err := store.client(t).TrafficEdges(context.Background(), TrafficQuery{
		Since: until,
		Until: until.Add(-time.Hour),
	}); err == nil {
		t.Fatal("a backwards window should be refused before it reaches the store")
	}
}

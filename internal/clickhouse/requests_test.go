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

// An insert names the table's own columns, not the API's. They differ in three
// places — `Timestamp`, `duration_ms`, `trace_id` — and a JSONEachRow row whose
// keys do not match is not an error ClickHouse reports: unknown keys are
// skipped and the columns take their defaults, so the rows land, empty.
func TestInsertRequestsWritesTheTablesOwnColumnNames(t *testing.T) {
	store := newFakeLogStore(t)

	at := time.Date(2026, 8, 15, 12, 0, 0, 123456789, time.UTC)
	err := store.client(t).InsertRequests(context.Background(), []Request{{
		Timestamp:   at,
		Project:     testProject,
		Environment: testEnvironment,
		Host:        "shop.example.com",
		Method:      "GET",
		Path:        "/works/12345",
		Route:       "/works/:id",
		Status:      500,
		DurationMs:  42.5,
		Protocol:    "HTTP/1.1",
		Source:      RequestSourceGateway,
		TraceID:     "abc",
	}})
	if err != nil {
		t.Fatalf("InsertRequests: %v", err)
	}

	if !strings.HasPrefix(store.query, "INSERT INTO "+qualified(RequestsTable)+" FORMAT JSONEachRow") {
		t.Fatalf("unexpected statement:\n%s", store.query)
	}
	for _, column := range []string{
		`"Timestamp":"2026-08-15 12:00:00.123456789"`, `"project":"shop"`,
		`"environment":"production"`, `"host":"shop.example.com"`, `"method":"GET"`,
		`"path":"/works/12345"`, `"route":"/works/:id"`, `"status":500`,
		`"duration_ms":42.5`, `"protocol":"HTTP/1.1"`, `"source":"gateway"`,
		`"trace_id":"abc"`,
	} {
		if !strings.Contains(store.query, column) {
			t.Errorf("expected %s in the row:\n%s", column, store.query)
		}
	}
}

// The raw table is DateTime64(9), and a timestamp written at millisecond
// resolution would throw away the precision the column exists to keep — two
// requests inside one millisecond are two requests.
func TestInsertRequestsKeepsNanosecondPrecision(t *testing.T) {
	store := newFakeLogStore(t)

	at := time.Date(2026, 8, 15, 12, 0, 0, 987654321, time.UTC)
	if err := store.client(t).InsertRequests(context.Background(), []Request{{Timestamp: at}}); err != nil {
		t.Fatalf("InsertRequests: %v", err)
	}
	if !strings.Contains(store.query, "12:00:00.987654321") {
		t.Errorf("the nanoseconds did not survive the insert:\n%s", store.query)
	}
}

// A row that names no vantage point came from the only one there is. Letting
// it through empty would put a blank value in the source facet forever, since
// nothing rewrites a row once it is written.
func TestInsertRequestsDefaultsTheVantagePoint(t *testing.T) {
	store := newFakeLogStore(t)

	if err := store.client(t).InsertRequests(context.Background(), []Request{{
		Project: testProject, Path: "/",
	}}); err != nil {
		t.Fatalf("InsertRequests: %v", err)
	}
	if !strings.Contains(store.query, `"source":"gateway"`) {
		t.Errorf("expected the gateway source to be filled in:\n%s", store.query)
	}
}

func TestInsertRequestsSendsNothingForAnEmptyBatch(t *testing.T) {
	store := newFakeLogStore(t)

	if err := store.client(t).InsertRequests(context.Background(), nil); err != nil {
		t.Fatalf("InsertRequests: %v", err)
	}
	if len(store.queries) != 0 {
		t.Errorf("expected no statement, got:\n%s", store.transcript())
	}
}

func TestQueryRequestsRefusesAnUnscopedListing(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{}); err == nil {
		t.Fatal("a listing with no project should be refused before it reaches the store")
	}
	if len(store.queries) != 0 {
		t.Errorf("expected no statement to be sent, got:\n%s", store.transcript())
	}
}

func TestQueryRequestsReadsARowIntoARequest(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-15T12:00:00.123456Z","project":"shop","environment":"production",` +
		`"host":"shop.example.com","method":"GET","path":"/works/12345","route":"/works/:id",` +
		`"status":500,"durationMs":42.5,"protocol":"HTTP/1.1","source":"gateway","traceId":"abc"}`

	requests, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{
		Project:     testProject,
		Environment: testEnvironment,
	})
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("want one request, got %d", len(requests))
	}

	request := requests[0]
	if !request.Timestamp.Equal(time.Date(2026, 8, 15, 12, 0, 0, 123456000, time.UTC)) {
		t.Errorf("unexpected timestamp: %s", request.Timestamp)
	}
	if request.Status != 500 || request.DurationMs != 42.5 {
		t.Errorf("unexpected answer: %+v", request)
	}
	if request.Path != "/works/12345" || request.Route != "/works/:id" {
		t.Errorf("the path and its template are both kept: %+v", request)
	}
	// A request list reads newest first — unlike a log, which reads forwards.
	if !strings.Contains(store.query, "ORDER BY Timestamp DESC") {
		t.Errorf("expected the newest requests first:\n%s", store.query)
	}
}

// Everything a caller typed travels as a parameter. The route in particular
// comes back out of a URL the user clicked, so it is the one most likely to
// carry something interesting.
func TestQueryRequestsPassesItsFiltersAsParameters(t *testing.T) {
	store := newFakeLogStore(t)

	_, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{
		Project:     testProject,
		Environment: testEnvironment,
		Route:       "/works/:id'; DROP TABLE http_requests; --",
		Method:      "GET",
		StatusClass: 5,
		OnlyErrors:  true,
	})
	if err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the route reached the query text:\n%s", store.query)
	}
	if got := store.params.Get("param_route"); !strings.Contains(got, "DROP TABLE") {
		t.Fatalf("the route should travel as a parameter, got %q", got)
	}
	if got := store.params.Get("param_statusClass"); got != "5" {
		t.Errorf("the status class should travel as a parameter, got %q", got)
	}
	if !strings.Contains(store.query, "intDiv(status, 100) = {statusClass:UInt8}") {
		t.Errorf("expected the status class to be matched on its leading digit:\n%s", store.query)
	}
	if !strings.Contains(store.query, requestErrorCondition) {
		t.Errorf("expected the error filter to be what the signals count:\n%s", store.query)
	}
}

func TestQueryRequestsCapsTheLimit(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{
		Project: testProject,
		Limit:   MaxRequestLimit * 10,
	}); err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if got := store.params.Get("param_limit"); got != "5000" {
		t.Errorf("the limit should be capped at %d, got %q", MaxRequestLimit, got)
	}
}

// The crash report asks what the edge was serving when a container died, which
// is a window either side of one instant rather than a window ending at it.
func TestCorrelatedRequestsCentresItsWindowOnTheMoment(t *testing.T) {
	store := newFakeLogStore(t)

	at := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if _, err := store.client(t).CorrelatedRequests(context.Background(), RequestCorrelationQuery{
		Project:     testProject,
		Environment: testEnvironment,
		At:          at,
		Within:      time.Minute,
		OnlyErrors:  true,
	}); err != nil {
		t.Fatalf("CorrelatedRequests: %v", err)
	}

	since, err := time.Parse(time.RFC3339Nano, store.params.Get("param_since"))
	if err != nil {
		t.Fatalf("reading the window's start: %v", err)
	}
	until, err := time.Parse(time.RFC3339Nano, store.params.Get("param_until"))
	if err != nil {
		t.Fatalf("reading the window's end: %v", err)
	}
	if !since.Equal(at.Add(-time.Minute)) || !until.Equal(at.Add(time.Minute)) {
		t.Errorf("the window %s..%s is not centred on %s", since, until, at)
	}
	if !strings.Contains(store.query, requestErrorCondition) {
		t.Errorf("a crash report asks for the failures:\n%s", store.query)
	}
}

func TestInsertK8sEventsWritesTheTablesOwnColumns(t *testing.T) {
	store := newFakeLogStore(t)

	at := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	err := store.client(t).InsertK8sEvents(context.Background(), []K8sEvent{{
		Timestamp: at,
		Namespace: "kitchen-system",
		Kind:      "DaemonSet",
		Name:      "kitchen-collector",
		Reason:    "FailedCreate",
		Message:   `violates PodSecurity "baseline:latest"`,
		Count:     12,
		Node:      "node-1",
	}})
	if err != nil {
		t.Fatalf("InsertK8sEvents: %v", err)
	}

	if !strings.HasPrefix(store.query, "INSERT INTO "+qualified(K8sEventsTable)+" FORMAT JSONEachRow") {
		t.Fatalf("unexpected statement:\n%s", store.query)
	}
	for _, column := range []string{
		`"timestamp":"2026-08-15 03:00:00.000"`, `"namespace":"kitchen-system"`,
		`"kind":"DaemonSet"`, `"name":"kitchen-collector"`, `"reason":"FailedCreate"`,
		`"count":12`, `"node":"node-1"`,
		// The event that explains a collector nothing admitted belongs to no
		// project, and the empty value is written rather than left out.
		`"project":""`,
	} {
		if !strings.Contains(store.query, column) {
			t.Errorf("expected %s in the row:\n%s", column, store.query)
		}
	}
}

func TestQueryK8sEventsFiltersAsParametersAndReadsARow(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"ts":"2026-08-15T03:00:00.000000Z","project":"","environment":"",` +
		`"namespace":"kitchen-system","kind":"DaemonSet","name":"kitchen-collector",` +
		`"reason":"FailedCreate","message":"violates PodSecurity","count":12,"node":"node-1"}`

	events, err := store.client(t).QueryK8sEvents(context.Background(), K8sEventQuery{
		Reason: "FailedCreate'; DROP TABLE k8s_events; --",
		Search: "PodSecurity",
	})
	if err != nil {
		t.Fatalf("QueryK8sEvents: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the reason reached the query text:\n%s", store.query)
	}
	if got := store.params.Get("param_search"); got != "PodSecurity" {
		t.Errorf("the search should travel as a parameter, got %q", got)
	}
	if len(events) != 1 {
		t.Fatalf("want one event, got %d", len(events))
	}
	if events[0].Count != 12 || events[0].Node != "node-1" {
		t.Errorf("unexpected event: %+v", events[0])
	}
	if !events[0].Timestamp.Equal(time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("unexpected timestamp: %s", events[0].Timestamp)
	}
}

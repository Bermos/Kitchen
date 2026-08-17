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
	"net/http"
	"sort"
	"strings"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The cluster's own warnings, explorable.
//
// This is not the activity feed. `/events` is the platform's story — builds
// finishing, releases moving — written by the reconcilers about things Kitchen
// did. This one is the cluster's, written about things that happened to it:
// FailedScheduling, FailedCreate, FailedMount, OOMKilling. Kubernetes expires
// its own copy about an hour after the fact, which is what makes the recorded
// history the only way to answer "what happened at 03:00".
//
// Every other platform screen deep-links into this one, which is why the facets
// are computed here rather than left to the client: a screen that arrives
// filtered to one pod should be able to say what else that pod was complaining
// about without a second round trip.

// eventFacetLimit is how many values one facet carries. A facet is a way into
// the data rather than a census, and twenty reasons is already more than
// anybody reads.
const eventFacetLimit = 20

// platformEvents answers the Events explorer.
func (s *Server) platformEvents(w http.ResponseWriter, req *http.Request) {
	since, until, err := windowFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultK8sEventLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	query := clickhouse.K8sEventQuery{
		Project:     strings.TrimSpace(req.URL.Query().Get("project")),
		Environment: strings.TrimSpace(req.URL.Query().Get("environment")),
		Namespace:   strings.TrimSpace(req.URL.Query().Get("namespace")),
		Kind:        strings.TrimSpace(req.URL.Query().Get("kind")),
		Name:        strings.TrimSpace(req.URL.Query().Get("name")),
		Reason:      strings.TrimSpace(req.URL.Query().Get("reason")),
		// The full-text search is over the message, which is where the useful
		// half of a Kubernetes warning lives: the reason names the category and
		// the message names the object, the port, the policy.
		Search: strings.TrimSpace(req.URL.Query().Get("search")),
		Since:  since,
		Until:  until,
		Limit:  limit,
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	events, err := store.QueryK8sEvents(req.Context(), query)
	if err != nil {
		s.writeStoreError(w, err, "the cluster event query")
		return
	}

	body := platformEventsBody{
		Items:     itemsOf(events),
		Facets:    facetsOf(events),
		Truncated: len(events) >= limit,
	}
	// The node facet is deliberately kept even when empty: an event with no
	// node is one about an object rather than about a machine, and a facet
	// that disappears reads as a column the explorer does not have.
	writeJSON(w, http.StatusOK, body)
}

// platformEventsBody is the Events explorer.
type platformEventsBody struct {
	Items []clickhouse.K8sEvent `json:"items"`
	// Facets are what else is in this selection: the distinct reasons, kinds,
	// namespaces and nodes, with counts.
	Facets []eventFacet `json:"facets"`
	// Truncated says the window held at least as many events as the limit
	// asked for — which is what makes the facets a description of the page
	// rather than of the window, and the screen has to say so rather than
	// implying a census.
	Truncated bool `json:"truncated"`
}

// eventFacet is one field's distinct values in the current selection.
type eventFacet struct {
	Field  string            `json:"field"`
	Values []eventFacetValue `json:"values"`
}

type eventFacetValue struct {
	Value string `json:"value"`
	// Count is how many events carry it — the event rows, not the Kubernetes
	// occurrence counts they each stand for. Two facets that counted
	// differently would not add up to the same total.
	Count int `json:"count"`
}

// The fields the explorer facets on, in the order they are useful: what
// happened, to what kind of thing, where, and on which machine.
var eventFacetFields = []struct {
	name  string
	value func(clickhouse.K8sEvent) string
}{
	{"reason", func(event clickhouse.K8sEvent) string { return event.Reason }},
	{"kind", func(event clickhouse.K8sEvent) string { return event.Kind }},
	{"namespace", func(event clickhouse.K8sEvent) string { return event.Namespace }},
	{"node", func(event clickhouse.K8sEvent) string { return event.Node }},
}

// facetsOf counts the selection's distinct values.
//
// They are computed over the rows that came back rather than asked of the store
// as their own aggregate, which is a deliberate limitation and the reason the
// body carries `truncated`: this is the page's shape, not the window's. It is
// the right trade at this size — the explorer's page is a thousand events at
// most, and a second aggregate per field would be four more queries for a
// number nobody sums.
func facetsOf(events []clickhouse.K8sEvent) []eventFacet {
	facets := make([]eventFacet, 0, len(eventFacetFields))
	for _, field := range eventFacetFields {
		counts := map[string]int{}
		for _, event := range events {
			if value := field.value(event); value != "" {
				counts[value]++
			}
		}
		values := make([]eventFacetValue, 0, len(counts))
		for value, count := range counts {
			values = append(values, eventFacetValue{Value: value, Count: count})
		}
		sort.Slice(values, func(i, j int) bool {
			if values[i].Count != values[j].Count {
				return values[i].Count > values[j].Count
			}
			return values[i].Value < values[j].Value
		})
		if len(values) > eventFacetLimit {
			values = values[:eventFacetLimit]
		}
		facets = append(facets, eventFacet{Field: field.name, Values: values})
	}
	return facets
}

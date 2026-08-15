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
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Flow is one flow observation on its way into the store: who talked to whom,
// what Cilium decided about it, and — for HTTP flows — how it went. The
// source and destination are workload names (a Deployment, not a pod), which
// is the granularity a service map draws.
type Flow struct {
	Timestamp            time.Time
	Source               string
	SourceNamespace      string
	Destination          string
	DestinationNamespace string
	// Protocol is TCP, UDP, ICMP — or HTTP for L7-visible flows.
	Protocol string
	// Verdict is Cilium's: FORWARDED, DROPPED, ...
	Verdict string
	// HTTPStatus and LatencyMs are set on HTTP response flows only.
	HTTPStatus uint16
	LatencyMs  float64
}

// InsertFlows writes a batch of flow observations.
func (c *Client) InsertFlows(ctx context.Context, flows []Flow) error {
	if len(flows) == 0 {
		return nil
	}
	rows := make([]string, 0, len(flows))
	for _, flow := range flows {
		row, err := json.Marshal(map[string]any{
			"timestamp":            flow.Timestamp.UTC().Format("2006-01-02 15:04:05.000"),
			"source":               flow.Source,
			"sourceNamespace":      flow.SourceNamespace,
			"destination":          flow.Destination,
			"destinationNamespace": flow.DestinationNamespace,
			"protocol":             flow.Protocol,
			"verdict":              flow.Verdict,
			"httpStatus":           flow.HTTPStatus,
			"latencyMs":            flow.LatencyMs,
		})
		if err != nil {
			return err
		}
		rows = append(rows, string(row))
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(FlowsTable), strings.Join(rows, "\n"))
	return c.Exec(ctx, statement)
}

// TrafficQuery selects a window of the service map. The zero value answers
// the last hour, cluster-wide.
type TrafficQuery struct {
	// Since and Until bound the window. A zero Since means an hour before
	// Until; a zero Until means now.
	Since time.Time
	Until time.Time
	// Namespace narrows to edges touching one namespace — how "this
	// project's traffic" is asked for, via its app namespace.
	Namespace string
}

// TrafficEdge is one aggregated edge of the service map: everything one
// workload said to another inside the window.
type TrafficEdge struct {
	Source               string `json:"source"`
	SourceNamespace      string `json:"sourceNamespace,omitempty"`
	Destination          string `json:"destination"`
	DestinationNamespace string `json:"destinationNamespace,omitempty"`
	// Protocol is the edge's dominant protocol; HTTP wins over its own
	// transport so an L7-visible edge reads as HTTP.
	Protocol string `json:"protocol"`
	// Flows is every observation on the edge, RPS that count divided by the
	// window — honest about being flows per second for non-HTTP edges.
	Flows uint64  `json:"flows"`
	RPS   float64 `json:"rps"`
	// Errors counts HTTP 5xx answers; Drops counts flows Cilium refused.
	Errors uint64 `json:"errors"`
	Drops  uint64 `json:"drops"`
	// P95Ms is the 95th-percentile HTTP latency, 0 when the edge has no
	// L7 visibility.
	P95Ms float64 `json:"p95Ms"`
}

// protocolAlias is the name the edge's dominant protocol is selected under.
//
// It is deliberately not `protocol`: ClickHouse resolves a name in the SELECT
// list against the other aliases before the table's columns, so aliasing the
// derived value to `protocol` hides the column behind the expression that
// computes it. The `protocol` inside `quantileIf(...)` then becomes the whole
// `if(countIf(...) > 0, ...)` expression, substituted in, and the query fails
// with "Aggregate function countIf(...) is found inside another aggregate
// function" — an error naming a countIf the query never wrote there. That is
// what the traffic page answered with on every load. See timestampAlias in
// logs.go for the same trap in the other reader.
const protocolAlias = "edgeProtocol"

// trafficRow is the wire shape of one aggregated edge. Counts arrive as
// strings because ClickHouse renders UInt64 that way in JSON.
type trafficRow struct {
	Source               string  `json:"source"`
	SourceNamespace      string  `json:"sourceNamespace"`
	Destination          string  `json:"destination"`
	DestinationNamespace string  `json:"destinationNamespace"`
	Protocol             string  `json:"edgeProtocol"`
	Flows                string  `json:"flows"`
	Errors               string  `json:"errors"`
	Drops                string  `json:"drops"`
	P95Ms                float64 `json:"p95Ms"`
}

// TrafficEdges aggregates the service map for a window: one row per
// (source, destination) pair, with rates, errors, drops and p95 latency.
func (c *Client) TrafficEdges(ctx context.Context, query TrafficQuery) ([]TrafficEdge, error) {
	until := query.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := query.Since
	if since.IsZero() {
		since = until.Add(-time.Hour)
	}
	window := until.Sub(since).Seconds()
	if window <= 0 {
		return nil, fmt.Errorf("the traffic window must end after it starts")
	}

	conditions := []string{
		"timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')",
		"timestamp <= parseDateTime64BestEffort({until:String}, 3, 'UTC')",
	}
	params := map[string]string{
		"since": since.UTC().Format(time.RFC3339Nano),
		"until": until.UTC().Format(time.RFC3339Nano),
	}
	if query.Namespace != "" {
		conditions = append(conditions,
			"(sourceNamespace = {namespace:String} OR destinationNamespace = {namespace:String})")
		params["namespace"] = query.Namespace
	}

	// The busiest edges are the ones worth keeping when the limit bites, so
	// the ordering is on the count itself and not on `flows` — that alias is
	// the count rendered as a String, and String order puts 9 above 1000.
	statement := fmt.Sprintf(`SELECT
    source, sourceNamespace, destination, destinationNamespace,
    if(countIf(protocol = 'HTTP') > 0, 'HTTP', anyLast(protocol)) AS %s,
    toString(count()) AS flows,
    toString(countIf(httpStatus >= 500)) AS errors,
    toString(countIf(verdict != 'FORWARDED')) AS drops,
    quantileIf(0.95)(latencyMs, protocol = 'HTTP' AND latencyMs > 0) AS p95Ms
FROM %s.%s
WHERE %s
GROUP BY source, sourceNamespace, destination, destinationNamespace
ORDER BY count() DESC
LIMIT 500
FORMAT JSONEachRow`,
		protocolAlias, quoteIdentifier(c.cfg.Database), quoteIdentifier(FlowsTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	edges := []TrafficEdge{}
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := trafficRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable traffic row: %w", err)
		}
		flows, _ := strconv.ParseUint(row.Flows, 10, 64)
		errors, _ := strconv.ParseUint(row.Errors, 10, 64)
		drops, _ := strconv.ParseUint(row.Drops, 10, 64)
		edges = append(edges, TrafficEdge{
			Source:               row.Source,
			SourceNamespace:      row.SourceNamespace,
			Destination:          row.Destination,
			DestinationNamespace: row.DestinationNamespace,
			Protocol:             row.Protocol,
			Flows:                flows,
			RPS:                  float64(flows) / window,
			Errors:               errors,
			Drops:                drops,
			P95Ms:                row.P95Ms,
		})
	}
	return edges, nil
}

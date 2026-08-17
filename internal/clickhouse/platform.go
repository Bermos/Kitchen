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
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The same store read the other way round: across projects rather than within
// one, for the operator who owns the platform instead of an application on it.
//
// These reads do not lead with a project, which is the one thing every other
// query in this package does. They are bounded by the window instead — the
// tables are partitioned by date, so a day-wide question reads a day's parts —
// and they exist because "several projects degrading at once is a platform
// problem wearing project costumes" is not a question a project-scoped
// endpoint can ever be asked.

// PlatformRequestsQuery is the window a cross-project read covers.
type PlatformRequestsQuery struct {
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// Limit caps the rows a listing form of the question answers with. The
	// headline aggregate has one row and ignores it.
	Limit int
}

// PlatformRequests is the edge's headline: everything that entered the
// platform in the window, however it was attributed.
type PlatformRequests struct {
	// Since and Until are the window that was actually answered; see
	// RequestSummary's, which are snapped the same way.
	Since             time.Time `json:"since"`
	Until             time.Time `json:"until"`
	Requests          uint64    `json:"requests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	Errors            uint64    `json:"errors"`
	ErrorRate         float64   `json:"errorRate"`
	P50Ms             float64   `json:"p50Ms"`
	P95Ms             float64   `json:"p95Ms"`
	P99Ms             float64   `json:"p99Ms"`
	// Unrouted is the part of Requests that asked for a host the platform
	// never published. It is counted here rather than filtered out because a
	// number that quietly dropped it would disagree with what the edge served.
	Unrouted uint64 `json:"unrouted"`
	Rollup   string `json:"rollup"`
}

// PlatformRequests aggregates every project's traffic into one answer.
func (c *Client) PlatformRequests(ctx context.Context, query PlatformRequestsQuery) (PlatformRequests, error) {
	scope, err := platformScope(query.Since, query.Until, requestRollupAuto)
	if err != nil {
		return PlatformRequests{}, err
	}

	statement := fmt.Sprintf(`SELECT
    %s,
    toString(countMergeIf(r.requests, r.project = '')) AS unrouted
FROM %s.%s AS r
WHERE %s
FORMAT JSONEachRow`,
		rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table), scope.where())

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return PlatformRequests{}, err
	}

	platform := PlatformRequests{Since: scope.since, Until: scope.until, Rollup: scope.rollup}
	if len(rows) > 0 {
		row := rows[0]
		platform.Requests = parseUint(row["requests"])
		platform.Errors = parseUint(row["errors"])
		platform.Unrouted = parseUint(row["unrouted"])
		platform.P50Ms = parseFloat(row["p50"])
		platform.P95Ms = parseFloat(row["p95"])
		platform.P99Ms = parseFloat(row["p99"])
	}
	if seconds := scope.seconds(); seconds > 0 {
		platform.RequestsPerSecond = float64(platform.Requests) / seconds
	}
	if platform.Requests > 0 {
		platform.ErrorRate = float64(platform.Errors) / float64(platform.Requests)
	}
	return platform, nil
}

// What a platform breakdown groups on. Routes and hosts answer "what is the
// edge busiest with"; environments answer "which application is slow", which
// is the latency-leaders table.
const (
	EdgeByRoute       = "route"
	EdgeByHost        = "host"
	EdgeByEnvironment = "environment"
)

// edgeGroupColumns is what each grouping actually reads. It is a fixed map
// because the name comes from a request and the column reaches the statement
// as written.
var edgeGroupColumns = map[string]string{
	EdgeByRoute:       "r.route",
	EdgeByHost:        "r.host",
	EdgeByEnvironment: "r.environment",
}

// EdgeBreakdownQuery is the Edge screen's tables, which are one query with
// different orderings: top routes and hosts by traffic, the same two by error
// rate, and the environments with the worst latency.
type EdgeBreakdownQuery struct {
	Since time.Time
	Until time.Time
	// By is one of the EdgeBy constants, defaulting to the route.
	By string
	// SortBy is one of the RouteSort constants, defaulting to traffic.
	SortBy string
	// MinRequests drops rows too quiet to rank. Sorting by error rate without
	// it puts a host that was asked for once, and failed, above an application
	// that is failing a tenth of a million requests.
	MinRequests uint64
	Limit       int
}

// EdgeEntry is one row of a platform breakdown: a route, a host or an
// environment, and what it served.
//
// Project and Environment name where the row belongs, and are both empty for
// the unrouted bucket — a host the platform never published still reached the
// edge, and hiding it from the traffic tables would be hiding the very traffic
// that needs explaining.
type EdgeEntry struct {
	Key               string  `json:"key"`
	Project           string  `json:"project,omitempty"`
	Environment       string  `json:"environment,omitempty"`
	Requests          uint64  `json:"requests"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
	Errors            uint64  `json:"errors"`
	ErrorRate         float64 `json:"errorRate"`
	P50Ms             float64 `json:"p50Ms"`
	P95Ms             float64 `json:"p95Ms"`
	P99Ms             float64 `json:"p99Ms"`
}

// EdgeBreakdown ranks the platform's traffic by whichever dimension is asked
// for.
func (c *Client) EdgeBreakdown(ctx context.Context, query EdgeBreakdownQuery) ([]EdgeEntry, error) {
	group := query.By
	if group == "" {
		group = EdgeByRoute
	}
	column, ok := edgeGroupColumns[group]
	if !ok {
		groups := make([]string, 0, len(edgeGroupColumns))
		for name := range edgeGroupColumns {
			groups = append(groups, name)
		}
		slices.Sort(groups)
		return nil, fmt.Errorf("cannot break the edge down by %q; the groupings are %s",
			query.By, strings.Join(groups, ", "))
	}
	order, err := requestSortExpression(query.SortBy)
	if err != nil {
		return nil, err
	}
	scope, err := platformScope(query.Since, query.Until, requestRollupAuto)
	if err != nil {
		return nil, err
	}

	limit := query.Limit
	if limit < 1 {
		limit = DefaultRequestGroupLimit
	}
	if limit > MaxRequestGroupLimit {
		limit = MaxRequestGroupLimit
	}
	scope.params["limit"] = strconv.Itoa(limit)
	scope.params["minRequests"] = strconv.FormatUint(query.MinRequests, 10)

	// The HAVING reads the aggregate and not the alias it is selected under,
	// which is a String and would compare lexicographically.
	statement := fmt.Sprintf(`SELECT
    %s AS key,
    r.project AS project,
    r.environment AS environment,
    %s
FROM %s.%s AS r
WHERE %s
GROUP BY key, project, environment
HAVING %s >= {minRequests:UInt64}
ORDER BY %s DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		column, rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table),
		scope.where(), rollupRequests, order)

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return nil, err
	}

	seconds := scope.seconds()
	entries := make([]EdgeEntry, 0, len(rows))
	for _, row := range rows {
		entry := EdgeEntry{
			Key:         row["key"],
			Project:     row["project"],
			Environment: row["environment"],
			Requests:    parseUint(row["requests"]),
			Errors:      parseUint(row["errors"]),
			P50Ms:       parseFloat(row["p50"]),
			P95Ms:       parseFloat(row["p95"]),
			P99Ms:       parseFloat(row["p99"]),
		}
		if seconds > 0 {
			entry.RequestsPerSecond = float64(entry.Requests) / seconds
		}
		if entry.Requests > 0 {
			entry.ErrorRate = float64(entry.Errors) / float64(entry.Requests)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// UnroutedHost is one host that reached the edge which the platform never
// published: a stale DNS record still pointing here, a scanner walking the
// address, or a custom domain whose Domain object was removed while its
// record was not.
//
// FirstSeen and LastSeen are what separate those cases. A host seen once an
// hour ago is noise; one seen continuously since a deploy is a route that
// stopped being published.
type UnroutedHost struct {
	Host              string    `json:"host"`
	Requests          uint64    `json:"requests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	FirstSeen         time.Time `json:"firstSeen"`
	LastSeen          time.Time `json:"lastSeen"`
}

// UnroutedHosts reads the unrouted bucket: the rows the follower could not
// attribute to any environment.
//
// It reads the raw table rather than a rollup, because the question is about a
// handful of hosts over hours rather than about a rate over months, and
// because the raw row is where the path is — which is what tells a scanner
// asking for `/wp-login.php` apart from a real client on a stale name.
func (c *Client) UnroutedHosts(ctx context.Context, query PlatformRequestsQuery) ([]UnroutedHost, error) {
	since, until, err := resolveWindow(query.Since, query.Until)
	if err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit < 1 {
		limit = DefaultRequestGroupLimit
	}
	if limit > MaxRequestGroupLimit {
		limit = MaxRequestGroupLimit
	}

	params := map[string]string{
		"since": since.Format(time.RFC3339Nano),
		"until": until.Format(time.RFC3339Nano),
		"limit": strconv.Itoa(limit),
	}
	statement := fmt.Sprintf(`SELECT
    host,
    toString(count()) AS requests,
    toString(toUnixTimestamp(min(Timestamp))) AS first,
    toString(toUnixTimestamp(max(Timestamp))) AS last
FROM %s.%s
WHERE project = '' AND %s AND %s
GROUP BY host
ORDER BY count() DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(RequestsTable),
		requestSinceCondition, requestUntilCondition)

	rows, err := c.selectionRows(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	seconds := until.Sub(since).Seconds()
	hosts := make([]UnroutedHost, 0, len(rows))
	for _, row := range rows {
		host := UnroutedHost{Host: row["host"], Requests: parseUint(row["requests"])}
		if seconds > 0 {
			host.RequestsPerSecond = float64(host.Requests) / seconds
		}
		if unix, err := strconv.ParseInt(row["first"], 10, 64); err == nil {
			host.FirstSeen = time.Unix(unix, 0).UTC()
		}
		if unix, err := strconv.ParseInt(row["last"], 10, 64); err == nil {
			host.LastSeen = time.Unix(unix, 0).UTC()
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// ProjectTrafficQuery asks what each project served.
//
// This is what `/metrics/overview` reads instead of aggregating flows by
// destination namespace. The flows answer misattributes a protected preview to
// the forward-auth gate and an idling environment to the KEDA interceptor,
// because both are the destination of the flow; requests are attributed by the
// host they asked for, which every hop preserves.
type ProjectTrafficQuery struct {
	Since time.Time
	Until time.Time
	// Project narrows to one; empty is every project, which is what the
	// platform overview asks for.
	Project string
	// ByEnvironment gives one row per environment rather than per project,
	// which is what a project page's per-environment cards want.
	ByEnvironment bool
	// Sparkline fills RequestsPerHour, at the cost of a second aggregate.
	Sparkline bool
	Limit     int
}

// ProjectTraffic is one project's — or one environment's — edge traffic over
// the window.
type ProjectTraffic struct {
	Project     string `json:"project"`
	Environment string `json:"environment,omitempty"`

	Requests          uint64  `json:"requests"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
	Errors            uint64  `json:"errors"`
	ErrorRate         float64 `json:"errorRate"`
	P50Ms             float64 `json:"p50Ms"`
	P95Ms             float64 `json:"p95Ms"`
	P99Ms             float64 `json:"p99Ms"`

	// RequestsPerHour is the sparkline, oldest first, one entry per hour from
	// the window's start truncated to the hour. Empty unless the query asked
	// for it.
	RequestsPerHour []uint64 `json:"requestsPerHour,omitempty"`
}

// ProjectTraffic answers per-project traffic for the dashboard's overview.
//
// The percentiles are merged over the whole window rather than averaged across
// its hours, which the sparkline's own grouping cannot do: a mean of hourly
// p95s is not a p95. That is why the sparkline is a second statement instead
// of a second column.
func (c *Client) ProjectTraffic(ctx context.Context, query ProjectTrafficQuery) ([]ProjectTraffic, error) {
	// The hour rollup, because these numbers are hourly at their finest and
	// the day-scale windows the overview asks for are sixty times cheaper
	// there. Both rollups hold the same states, so the answer is the same.
	scope, err := platformScope(query.Since, query.Until, RequestRollupHour)
	if err != nil {
		return nil, err
	}
	// The unrouted bucket is traffic, but it is not a project's, and a row
	// keyed on the empty project would sit in the overview looking like one.
	scope.conditions = append(scope.conditions, "r.project != ''")
	if query.Project != "" {
		scope.conditions = append(scope.conditions, "r.project = {project:String}")
		scope.params["project"] = query.Project
	}

	limit := query.Limit
	if limit < 1 {
		limit = DefaultRequestGroupLimit
	}
	if limit > MaxRequestGroupLimit {
		limit = MaxRequestGroupLimit
	}
	scope.params["limit"] = strconv.Itoa(limit)

	group := "project"
	columns := "r.project AS project"
	if query.ByEnvironment {
		group = "project, environment"
		columns += ",\n    r.environment AS environment"
	}

	statement := fmt.Sprintf(`SELECT
    %s,
    %s
FROM %s.%s AS r
WHERE %s
GROUP BY %s
ORDER BY %s DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		columns, rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table),
		scope.where(), group, rollupRequests)

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return nil, err
	}

	seconds := scope.seconds()
	traffic := make([]ProjectTraffic, 0, len(rows))
	for _, row := range rows {
		entry := ProjectTraffic{
			Project:     row["project"],
			Environment: row["environment"],
			Requests:    parseUint(row["requests"]),
			Errors:      parseUint(row["errors"]),
			P50Ms:       parseFloat(row["p50"]),
			P95Ms:       parseFloat(row["p95"]),
			P99Ms:       parseFloat(row["p99"]),
		}
		if seconds > 0 {
			entry.RequestsPerSecond = float64(entry.Requests) / seconds
		}
		if entry.Requests > 0 {
			entry.ErrorRate = float64(entry.Errors) / float64(entry.Requests)
		}
		traffic = append(traffic, entry)
	}
	if !query.Sparkline {
		return traffic, nil
	}
	return traffic, c.fillProjectSparklines(ctx, scope, group, columns, traffic)
}

// fillProjectSparklines adds the hourly series to rows the totals already
// found. It is keyed the same way the totals are grouped, so a project that
// the limit dropped is not queried for and a row that came back is filled.
func (c *Client) fillProjectSparklines(
	ctx context.Context, scope requestScope, group, columns string, traffic []ProjectTraffic,
) error {
	// The scope's start is already on an hour, since this reads the hour
	// rollup and every rollup read is snapped to its resolution.
	start := scope.since
	hours := int(scope.until.Sub(start)/time.Hour) + 1
	if hours < 1 {
		return nil
	}

	statement := fmt.Sprintf(`SELECT
    %s,
    toString(toUnixTimestamp(toStartOfHour(r.bucket))) AS hour,
    toString(%s) AS requests
FROM %s.%s AS r
WHERE %s
GROUP BY %s, hour
FORMAT JSONEachRow`,
		columns, rollupRequests, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table),
		scope.where(), group)

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return err
	}

	// The two halves of a row's key are joined by a byte neither a project nor
	// an environment name can contain, so a project "a-b" and a project "a"
	// with an environment "b" cannot land on the same entry.
	index := map[string]*ProjectTraffic{}
	for i := range traffic {
		traffic[i].RequestsPerHour = make([]uint64, hours)
		index[traffic[i].Project+"\x00"+traffic[i].Environment] = &traffic[i]
	}
	for _, row := range rows {
		entry := index[row["project"]+"\x00"+row["environment"]]
		if entry == nil {
			continue
		}
		if i, ok := bucketIndex(row["hour"], start, time.Hour, hours); ok {
			entry.RequestsPerHour[i] = parseUint(row["requests"])
		}
	}
	return nil
}

// NodeFreshness is when the store last received anything a node's collector
// produced.
type NodeFreshness struct {
	Node     string    `json:"node"`
	LastSeen time.Time `json:"lastSeen"`
}

// TelemetryFreshness answers, per node, when the store last saw a row from it.
//
// It is the only way a dead collector looks broken. A node whose collector was
// never admitted — the PodSecurity failure the platform namespace's level
// exists to prevent — has no pods at all, so every screen that counts pods
// looks clean and every screen that reads its telemetry silently shows less.
//
// A node silent for longer than the lookback is simply absent from the answer
// rather than reported with an old timestamp; the caller holds the node list
// from the API server and reads an absence as silence. That keeps the query
// bounded to a window instead of scanning the whole retention to prove a
// negative.
func (c *Client) TelemetryFreshness(ctx context.Context, within time.Duration) ([]NodeFreshness, error) {
	if within <= 0 {
		within = time.Hour
	}
	since := time.Now().UTC().Add(-within)
	params := map[string]string{"since": since.Format(time.RFC3339Nano)}

	// Two sources, because a node can be silent in one and not the other: the
	// logs stop when the filelog receiver does, the metrics when the kubelet
	// scraper does, and a collector that half-works is still a collector to
	// look at. Both branches reduce to unix seconds so the union has one type
	// rather than a DateTime and a DateTime64 to reconcile.
	statement := fmt.Sprintf(`SELECT
    node,
    toString(max(seen)) AS lastSeen
FROM (
    SELECT node, toUnixTimestamp(max(%[3]s)) AS seen
    FROM %[1]s.%[2]s
    WHERE node != '' AND %[3]s >= parseDateTime64BestEffort({since:String}, 9, 'UTC')
    GROUP BY node
    UNION ALL
    SELECT node, toUnixTimestamp(max(%[5]s)) AS seen
    FROM %[1]s.%[4]s
    WHERE node != '' AND %[5]s >= parseDateTimeBestEffort({since:String}, 'UTC')
    GROUP BY node
)
GROUP BY node
ORDER BY node
FORMAT JSONEachRow`,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(LogsTable), timeColumnLogs,
		quoteIdentifier(MetricsGaugeTable), timeColumnMetrics)

	rows, err := c.selectionRows(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	freshness := make([]NodeFreshness, 0, len(rows))
	for _, row := range rows {
		unix, err := strconv.ParseInt(row["lastSeen"], 10, 64)
		if err != nil {
			continue
		}
		freshness = append(freshness, NodeFreshness{
			Node:     row["node"],
			LastSeen: time.Unix(unix, 0).UTC(),
		})
	}
	return freshness, nil
}

// platformScope is requestScope without the project, for the reads that are
// about the platform rather than about something on it.
func platformScope(since, until time.Time, rollup string) (requestScope, error) {
	resolvedSince, resolvedUntil, err := resolveWindow(since, until)
	if err != nil {
		return requestScope{}, err
	}
	if rollup == requestRollupAuto {
		rollup = rollupForSpan(resolvedUntil.Sub(resolvedSince))
	}
	resolvedSince = snapToRollup(resolvedSince, rollup)
	scope := requestScope{
		since:  resolvedSince,
		until:  resolvedUntil,
		rollup: rollup,
		table:  RequestsMinuteTable,
		conditions: []string{
			"r.bucket >= parseDateTimeBestEffort({since:String}, 'UTC')",
			"r.bucket <= parseDateTimeBestEffort({until:String}, 'UTC')",
		},
		params: map[string]string{
			"since": resolvedSince.Format(time.RFC3339Nano),
			"until": resolvedUntil.Format(time.RFC3339Nano),
		},
	}
	if rollup == RequestRollupHour {
		scope.table = RequestsHourTable
	}
	return scope, nil
}

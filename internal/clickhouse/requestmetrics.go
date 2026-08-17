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

// The golden signals — traffic, errors, latency — read off the rollups rather
// than off the rows they were computed from. A busy environment writes
// millions of raw rows a day and a few thousand buckets, and a chart wants the
// buckets.
//
// The rollups keep aggregate states rather than numbers, which is what makes
// this arithmetic honest: merging a day of minutes into an hour is the same
// answer as bucketing by the hour directly, and a percentile is only mergeable
// at all as a state. Averaging per-minute p95s, which is what a table of
// pre-computed numbers would force, is not a p95 of anything.

// The rollups, named for what a read reports having used.
const (
	RequestRollupMinute = "1m"
	RequestRollupHour   = "1h"
)

// requestRollupAuto asks a scope to pick the rollup from the window's own
// length, which is what every read without buckets of its own does.
const requestRollupAuto = ""

// requestHourlyThreshold is the window past which an aggregate read moves to
// the hour rollup. Two days is where the minute rollup stops being cheap and
// starts being merely affordable; nothing on the environment page asks for
// more, and the operator's year-scale views ask for much more.
const requestHourlyThreshold = 48 * time.Hour

// requestBucketLadder is the ladder a request series is quantised to, so that
// panning the window does not restripe the chart at some arbitrary width.
//
// It starts at a minute because that is the finest rollup there is, and every
// rung from an hour up is a whole number of hours, so the hour rollup buckets
// evenly into all of them.
var requestBucketLadder = []int{
	60, 120, 300, 600, 900, 1800,
	3600, 7200, 10800, 21600, 43200, 86400,
}

// DefaultRequestBuckets is how many points a series is drawn with when the
// caller does not say, and MaxRequestBuckets the ceiling.
const (
	DefaultRequestBuckets = 60
	MaxRequestBuckets     = 480
)

// DefaultRequestGroupLimit is how many rows a grouped answer — routes, hosts,
// leaders — returns by default, and MaxRequestGroupLimit the ceiling. The
// route budget caps an environment at 300 templates, so the ceiling is past
// every route an environment can have.
const (
	DefaultRequestGroupLimit = 100
	MaxRequestGroupLimit     = 500
)

// The aggregates every rollup read is built from.
//
// Every argument is qualified with `r`, the alias each read gives its table,
// and that is load-bearing rather than tidy: the state columns are called
// `requests` and `duration`, the values selected out of them are called the
// same, and ClickHouse resolves a bare name against the SELECT list's own
// aliases before the table's columns. See createMetricsRollupGaugeView, where
// the same shadowing makes the statement fail outright.
const (
	rollupRequests  = "countMerge(r.requests)"
	rollupErrors    = "countMergeIf(r.requests, r.status >= 500)"
	rollupQuantiles = "quantilesTDigestMerge(0.5, 0.95, 0.99)(r.duration)"
)

// requestErrorCondition is what the golden signals count as an error, over the
// raw table. A 4xx is the caller's fault and belongs in the route table's
// status breakdown, not in the number that says the service is broken.
const requestErrorCondition = "status >= 500"

// rollupQuantile picks one percentile out of the merged digest.
//
// The guard is not decoration: a merge over no rows answers nan, and a nan
// reaches the API as a JSON number that does not exist — `json.Marshal`
// refuses it. Zero is also the honest reading, since a window with no requests
// in it has no latency.
func rollupQuantile(index int) string {
	return fmt.Sprintf("ifNotFinite(%s[%d], 0)", rollupQuantiles, index)
}

// rollupSignals is the projection every golden-signal read selects: the three
// signals, cast to String for one decoding path.
var rollupSignals = strings.Join([]string{
	fmt.Sprintf("toString(%s) AS requests", rollupRequests),
	fmt.Sprintf("toString(%s) AS errors", rollupErrors),
	fmt.Sprintf("toString(%s) AS p50", rollupQuantile(1)),
	fmt.Sprintf("toString(%s) AS p95", rollupQuantile(2)),
	fmt.Sprintf("toString(%s) AS p99", rollupQuantile(3)),
}, ",\n    ")

// RequestQuery scopes a golden-signal read. Project is required; an empty
// Environment reads the whole project, which is what a project's own header
// asks for.
type RequestQuery struct {
	Project     string
	Environment string
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// Route narrows every number to one route template, which is what
	// clicking a row of the route table does to the charts beside it.
	Route string
}

// requestScope is a resolved read: the window it covers, the rollup answering
// it, and the predicate that selects it.
type requestScope struct {
	since, until time.Time
	rollup       string
	table        string
	conditions   []string
	params       map[string]string
}

// seconds is the window's length, which is what a rate is per.
func (s requestScope) seconds() float64 {
	return s.until.Sub(s.since).Seconds()
}

// where renders the scope's predicate.
func (s requestScope) where() string {
	return strings.Join(s.conditions, " AND ")
}

// scope resolves the query against one of the rollups, or against whichever
// one the window's length calls for.
//
// Every column is read through the table's alias for the reason the aggregates
// above are: `bucket` is both a column and the name the bucketed value is
// selected under, and ClickHouse would resolve the bare name in the WHERE
// clause to the alias. That is the same trap timestampAlias documents, where
// it merely made the log view fail on its first query.
func (q RequestQuery) scope(rollup string) (requestScope, error) {
	if strings.TrimSpace(q.Project) == "" {
		return requestScope{}, fmt.Errorf("a request read must name a project")
	}
	since, until, err := resolveWindow(q.Since, q.Until)
	if err != nil {
		return requestScope{}, err
	}
	if rollup == requestRollupAuto {
		rollup = rollupForSpan(until.Sub(since))
	}

	scope := requestScope{
		since:  since,
		until:  until,
		rollup: rollup,
		table:  RequestsMinuteTable,
		conditions: []string{
			"r.project = {project:String}",
			"r.bucket >= parseDateTimeBestEffort({since:String}, 'UTC')",
			"r.bucket <= parseDateTimeBestEffort({until:String}, 'UTC')",
		},
		params: map[string]string{
			"project": q.Project,
			"since":   since.Format(time.RFC3339Nano),
			"until":   until.Format(time.RFC3339Nano),
		},
	}
	if rollup == RequestRollupHour {
		scope.table = RequestsHourTable
	}
	if q.Environment != "" {
		scope.conditions = append(scope.conditions, "r.environment = {environment:String}")
		scope.params["environment"] = q.Environment
	}
	if q.Route != "" {
		scope.conditions = append(scope.conditions, "r.route = {route:String}")
		scope.params["route"] = q.Route
	}
	return scope, nil
}

// rollupForSpan picks the rollup a read with no buckets of its own is answered
// from. The two hold the same states, so this changes what it costs and not
// what it says.
func rollupForSpan(span time.Duration) string {
	if span > requestHourlyThreshold {
		return RequestRollupHour
	}
	return RequestRollupMinute
}

// rollupForWidth picks the rollup a series is drawn from: its own buckets are
// never finer than the rollup's own, and an hour-wide bucket is the hour
// rollup's by definition.
func rollupForWidth(width int) string {
	if width >= 3600 {
		return RequestRollupHour
	}
	return RequestRollupMinute
}

// RequestSummary is the golden-signal header for a window: how much traffic,
// how much of it failed, and how slow it was.
type RequestSummary struct {
	Since             time.Time `json:"since"`
	Until             time.Time `json:"until"`
	Requests          uint64    `json:"requests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	// Errors is answers of 500 and above; ErrorRate is that over Requests, and
	// is zero rather than undefined for a window with no traffic.
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"errorRate"`
	P50Ms     float64 `json:"p50Ms"`
	P95Ms     float64 `json:"p95Ms"`
	P99Ms     float64 `json:"p99Ms"`
	// Rollup names which table answered, which is the honest way to explain
	// why a year-wide summary is cheap.
	Rollup string `json:"rollup"`
}

// RequestSummary aggregates one environment's window into the four tiles the
// environment page leads with.
func (c *Client) RequestSummary(ctx context.Context, query RequestQuery) (RequestSummary, error) {
	scope, err := query.scope(requestRollupAuto)
	if err != nil {
		return RequestSummary{}, err
	}

	statement := fmt.Sprintf(`SELECT
    %s
FROM %s.%s AS r
WHERE %s
FORMAT JSONEachRow`,
		rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table), scope.where())

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return RequestSummary{}, err
	}

	summary := RequestSummary{Since: scope.since, Until: scope.until, Rollup: scope.rollup}
	// An aggregate with no GROUP BY always answers one row, even over nothing;
	// a store that has never seen a request answers zeroes rather than an
	// empty result, which is what the empty screen wants to say anyway.
	if len(rows) > 0 {
		row := rows[0]
		summary.Requests = parseUint(row["requests"])
		summary.Errors = parseUint(row["errors"])
		summary.P50Ms = parseFloat(row["p50"])
		summary.P95Ms = parseFloat(row["p95"])
		summary.P99Ms = parseFloat(row["p99"])
	}
	if seconds := scope.seconds(); seconds > 0 {
		summary.RequestsPerSecond = float64(summary.Requests) / seconds
	}
	if summary.Requests > 0 {
		summary.ErrorRate = float64(summary.Errors) / float64(summary.Requests)
	}
	return summary, nil
}

// RequestSeriesQuery draws the same signals over time.
type RequestSeriesQuery struct {
	RequestQuery
	// Buckets is how many points are wanted. The width is rounded up to the
	// next rung of the ladder, so the answer usually has fewer.
	Buckets int
}

// RequestPoint is one bucket of a request series.
type RequestPoint struct {
	Start             time.Time `json:"start"`
	Requests          uint64    `json:"requests"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
	Errors            uint64    `json:"errors"`
	ErrorRate         float64   `json:"errorRate"`
	P50Ms             float64   `json:"p50Ms"`
	P95Ms             float64   `json:"p95Ms"`
	P99Ms             float64   `json:"p99Ms"`
}

// RequestSeries is a window of one environment's traffic, including the empty
// buckets: a gap is an environment that served nothing, and on the traffic
// chart that is the most interesting shape there is.
type RequestSeries struct {
	Start         time.Time      `json:"start"`
	End           time.Time      `json:"end"`
	BucketSeconds int            `json:"bucketSeconds"`
	Points        []RequestPoint `json:"points"`
	Rollup        string         `json:"rollup"`
}

// RequestSeries buckets the golden signals across a window.
func (c *Client) RequestSeries(ctx context.Context, query RequestSeriesQuery) (RequestSeries, error) {
	buckets := query.Buckets
	if buckets < 1 {
		buckets = DefaultRequestBuckets
	}
	if buckets > MaxRequestBuckets {
		buckets = MaxRequestBuckets
	}

	// The window is resolved before the width is chosen, because an open-ended
	// query still has a length and the width is picked from it.
	since, until, err := resolveWindow(query.Since, query.Until)
	if err != nil {
		return RequestSeries{}, err
	}
	width := requestBucketSeconds(until.Sub(since), buckets)

	scope, err := RequestQuery{
		Project:     query.Project,
		Environment: query.Environment,
		Since:       since,
		Until:       until,
		Route:       query.Route,
	}.scope(rollupForWidth(width))
	if err != nil {
		return RequestSeries{}, err
	}
	scope.params["width"] = strconv.Itoa(width)

	// ClickHouse's toStartOfInterval counts from the Unix epoch, so the buckets
	// filled in here have to as well. See LogHistogram for why Go's Truncate is
	// not the same alignment.
	start := time.Unix(since.Unix()-since.Unix()%int64(width), 0).UTC()
	step := time.Duration(width) * time.Second
	count := int(until.Sub(start)/step) + 1
	if count > MaxRequestBuckets {
		count = MaxRequestBuckets
	}
	series := RequestSeries{
		Start:         start,
		End:           until,
		BucketSeconds: width,
		Points:        make([]RequestPoint, count),
		Rollup:        scope.rollup,
	}
	for i := range series.Points {
		series.Points[i].Start = start.Add(time.Duration(i) * step)
	}

	// `slot` is deliberately not called `bucket`: it is grouped and ordered on,
	// and a GROUP BY resolves its name against the SELECT aliases first, so an
	// alias of that name would group by the rendered string rather than by the
	// time. resourceSeriesStatement carries the same alias for the same reason.
	statement := fmt.Sprintf(`SELECT
    toStartOfInterval(r.bucket, toIntervalSecond({width:UInt32})) AS slot,
    toString(toUnixTimestamp(slot)) AS bucket,
    %s
FROM %s.%s AS r
WHERE %s
GROUP BY slot
ORDER BY slot
FORMAT JSONEachRow`,
		rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table), scope.where())

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return RequestSeries{}, err
	}

	for _, row := range rows {
		seconds, err := strconv.ParseInt(row["bucket"], 10, 64)
		if err != nil {
			continue
		}
		i := int(time.Unix(seconds, 0).UTC().Sub(start) / step)
		if i < 0 || i >= len(series.Points) {
			continue
		}
		point := &series.Points[i]
		point.Requests = parseUint(row["requests"])
		point.Errors = parseUint(row["errors"])
		point.RequestsPerSecond = float64(point.Requests) / float64(width)
		if point.Requests > 0 {
			point.ErrorRate = float64(point.Errors) / float64(point.Requests)
		}
		point.P50Ms = parseFloat(row["p50"])
		point.P95Ms = parseFloat(row["p95"])
		point.P99Ms = parseFloat(row["p99"])
	}
	return series, nil
}

// The orders a route table may be taken in. They are names rather than
// expressions because the value comes from a request, and the expression it
// selects is chosen from a fixed map.
const (
	RouteSortRequests  = "requests"
	RouteSortErrors    = "errors"
	RouteSortErrorRate = "errorRate"
	RouteSortLatency   = "p95"
)

// requestSortExpressions is what each sort actually orders on.
//
// Every one of them is the aggregate itself and never the alias it is selected
// under: the counts are selected as Strings, and ordering on a String puts 9
// above 1000 — which is how the traffic view's 500-row limit once kept the
// quietest edges.
var requestSortExpressions = map[string]string{
	RouteSortRequests:  rollupRequests,
	RouteSortErrors:    rollupErrors,
	RouteSortErrorRate: fmt.Sprintf("%s / %s", rollupErrors, rollupRequests),
	RouteSortLatency:   rollupQuantile(2),
}

// requestSortExpression resolves a sort, or says what the choices were.
func requestSortExpression(sortBy string) (string, error) {
	if sortBy == "" {
		return requestSortExpressions[RouteSortRequests], nil
	}
	if expression, ok := requestSortExpressions[sortBy]; ok {
		return expression, nil
	}
	sorts := make([]string, 0, len(requestSortExpressions))
	for name := range requestSortExpressions {
		sorts = append(sorts, name)
	}
	slices.Sort(sorts)
	return "", fmt.Errorf("cannot sort by %q; the sorts are %s", sortBy, strings.Join(sorts, ", "))
}

// RequestRoutesQuery asks for the per-route breakdown of a window.
type RequestRoutesQuery struct {
	RequestQuery
	// SortBy is one of the RouteSort constants, defaulting to traffic. It
	// decides which rows survive the limit, so it is a query and not a
	// presentation detail.
	SortBy string
	Limit  int
}

// RequestRoute is one row of the route table: what one template served over
// the window.
type RequestRoute struct {
	Route             string  `json:"route"`
	Requests          uint64  `json:"requests"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
	Errors            uint64  `json:"errors"`
	ErrorRate         float64 `json:"errorRate"`
	P50Ms             float64 `json:"p50Ms"`
	P95Ms             float64 `json:"p95Ms"`
	P99Ms             float64 `json:"p99Ms"`
}

// RequestRoutes breaks a window down per route template.
//
// This is the per-path view the goals ask for, and it only works because the
// follower bounds the route set: a table grouped by raw paths would have a row
// per user id and no two requests would ever group.
func (c *Client) RequestRoutes(ctx context.Context, query RequestRoutesQuery) ([]RequestRoute, error) {
	order, err := requestSortExpression(query.SortBy)
	if err != nil {
		return nil, err
	}
	scope, err := query.RequestQuery.scope(requestRollupAuto)
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

	statement := fmt.Sprintf(`SELECT
    r.route AS route,
    %s
FROM %s.%s AS r
WHERE %s
GROUP BY route
ORDER BY %s DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		rollupSignals, quoteIdentifier(c.cfg.Database), quoteIdentifier(scope.table),
		scope.where(), order)

	rows, err := c.selectionRows(ctx, statement, scope.params)
	if err != nil {
		return nil, err
	}

	seconds := scope.seconds()
	routes := make([]RequestRoute, 0, len(rows))
	for _, row := range rows {
		route := RequestRoute{
			Route:    row["route"],
			Requests: parseUint(row["requests"]),
			Errors:   parseUint(row["errors"]),
			P50Ms:    parseFloat(row["p50"]),
			P95Ms:    parseFloat(row["p95"]),
			P99Ms:    parseFloat(row["p99"]),
		}
		if seconds > 0 {
			route.RequestsPerSecond = float64(route.Requests) / seconds
		}
		if route.Requests > 0 {
			route.ErrorRate = float64(route.Errors) / float64(route.Requests)
		}
		routes = append(routes, route)
	}
	return routes, nil
}

// requestBucketSeconds picks the narrowest rung of the ladder that fits the
// span into the requested number of points.
func requestBucketSeconds(span time.Duration, buckets int) int {
	wanted := int(span.Seconds()) / buckets
	for _, width := range requestBucketLadder {
		if width >= wanted {
			return width
		}
	}
	return requestBucketLadder[len(requestBucketLadder)-1]
}

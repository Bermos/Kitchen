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
	"strconv"
	"strings"
	"time"
)

// Resource telemetry: what an environment is actually using, over time.
//
// The workload endpoint answers the instant — replicas, restarts, the requests
// a release asked for — by reading the API server. This answers the history,
// which is a different question and needs a different store: "was it always
// using this much memory", "did it get OOMKilled overnight", "when did it
// scale". Those are unanswerable from the current state of a Deployment, and
// they are the reason any of this is collected at all.
//
// Nothing in this file writes. The kubelet's usage arrives through the
// collector and the operator's usage collector emits the rest over OTLP, so
// both halves land in the metric tables as ordinary OTel points and this is
// only their reader.

// resourceBucketLadder is the ladder a resource window is quantised to. It is
// the histogram's ladder trimmed at both ends: nothing below the collector's
// scrape interval, because a bucket narrower than that draws gaps that are
// only the scraper blinking, and every rung at or above five minutes is a
// multiple of it, because that is what makes the rollup summable into it.
var resourceBucketLadder = []int{
	30, 60, 120, 300, 600, 900, 1800,
	3600, 10800, 21600, 43200, 86400,
}

// DefaultResourceBuckets is how many points a series is drawn with when the
// caller does not say, and MaxResourceBuckets the ceiling.
const (
	DefaultResourceBuckets = 60
	MaxResourceBuckets     = 480
)

// ResourceSeriesQuery asks what one environment used over a window. Project
// and Environment are both required: this is a question about one workload,
// and the whole-platform version of it is the metrics overview.
type ResourceSeriesQuery struct {
	Project     string
	Environment string
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// Buckets is how many points are wanted. The width is rounded up to the
	// next rung of the ladder, so the answer usually has fewer.
	Buckets int
}

// ResourcePoint is one bucket of a resource series. CPU and memory are summed
// across the environment's containers, which is what "what is this environment
// using" means; Replicas is how many distinct pods reported in the bucket,
// which is the same question the autoscaler answers and is the only way to see
// an environment that idled to zero and came back.
//
// The peaks are the sum of each container's peak within the bucket. That is a
// ceiling rather than a coincident total — two containers need not peak at the
// same moment — and it is the useful direction to be wrong in for "did this
// ever come near its limit".
type ResourcePoint struct {
	Start           time.Time `json:"start"`
	CPUCores        float64   `json:"cpuCores"`
	CPUPeakCores    float64   `json:"cpuPeakCores"`
	MemoryBytes     uint64    `json:"memoryBytes"`
	MemoryPeakBytes uint64    `json:"memoryPeakBytes"`
	Replicas        uint64    `json:"replicas"`
	Restarts        uint64    `json:"restarts"`
	OOMKills        uint64    `json:"oomKills"`
}

// ResourceSeries is a window of one environment's usage, including the empty
// buckets: a gap is a scaled-to-zero environment or a collector that was not
// running, and both are worth seeing.
type ResourceSeries struct {
	Start         time.Time       `json:"start"`
	End           time.Time       `json:"end"`
	BucketSeconds int             `json:"bucketSeconds"`
	Points        []ResourcePoint `json:"points"`

	// The limits the environment ran under, as the newest sample in the
	// window reported them — what the usage should be read against.
	CPULimitCores    float64 `json:"cpuLimitCores"`
	MemoryLimitBytes uint64  `json:"memoryLimitBytes"`

	// Restarts and OOMKills over the whole window, so the page can say "3
	// restarts, 1 OOMKill" without re-adding the series.
	Restarts uint64 `json:"restarts"`
	OOMKills uint64 `json:"oomKills"`

	// Rollup reports which table answered: the five-minute rollup, or the
	// metric tables themselves. It is the honest way to explain why a wide
	// window has coarser points than the one that was asked for.
	Rollup bool `json:"rollup"`
}

// ResourceSeries buckets one environment's samples across a window.
//
// A bucket at or above the rollup's own width is answered from the rollup and
// anything narrower from the metric tables. The two agree — the rollup keeps
// aggregate states rather than pre-computed numbers, so merging five-minute
// buckets into an hour is the same arithmetic as bucketing the points into an
// hour directly.
func (c *Client) ResourceSeries(ctx context.Context, query ResourceSeriesQuery) (ResourceSeries, error) {
	if strings.TrimSpace(query.Project) == "" || strings.TrimSpace(query.Environment) == "" {
		return ResourceSeries{}, fmt.Errorf("a resource series must name a project and an environment")
	}

	until := query.Until
	if until.IsZero() {
		until = time.Now()
	}
	since := query.Since
	if since.IsZero() {
		since = until.Add(-time.Hour)
	}
	until, since = until.UTC(), since.UTC()
	if !until.After(since) {
		return ResourceSeries{}, fmt.Errorf("the resource window must end after it starts")
	}

	buckets := query.Buckets
	if buckets < 1 {
		buckets = DefaultResourceBuckets
	}
	if buckets > MaxResourceBuckets {
		buckets = MaxResourceBuckets
	}
	width := resourceBucketSeconds(until.Sub(since), buckets)

	// ClickHouse's toStartOfInterval counts from the Unix epoch, so the
	// buckets filled in here have to as well. See LogHistogram for the same
	// alignment and why Go's Truncate is not it.
	start := time.Unix(since.Unix()-since.Unix()%int64(width), 0).UTC()

	series := ResourceSeries{
		Start:         start,
		End:           until,
		BucketSeconds: width,
		Rollup:        width >= MetricsRollupSeconds,
	}
	count := int(until.Sub(start)/(time.Duration(width)*time.Second)) + 1
	if count > MaxResourceBuckets {
		count = MaxResourceBuckets
	}
	series.Points = make([]ResourcePoint, count)
	step := time.Duration(width) * time.Second
	for i := range series.Points {
		series.Points[i].Start = start.Add(time.Duration(i) * step)
	}

	params := map[string]string{
		"project":     query.Project,
		"environment": query.Environment,
		"since":       since.Format(time.RFC3339Nano),
		"until":       until.Format(time.RFC3339Nano),
		"width":       strconv.Itoa(width),
	}
	rows, err := c.selectionRows(ctx, resourceSeriesStatement(c.cfg.Database, series.Rollup), params)
	if err != nil {
		return ResourceSeries{}, err
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
		point.CPUCores = parseFloat(row["cpu"])
		point.CPUPeakCores = parseFloat(row["cpuPeak"])
		point.MemoryBytes = parseUint(row["memory"])
		point.MemoryPeakBytes = parseUint(row["memoryPeak"])
		point.Replicas = parseUint(row["replicas"])
		point.Restarts = parseUint(row["restarts"])
		point.OOMKills = parseUint(row["oomKills"])

		series.Restarts += point.Restarts
		series.OOMKills += point.OOMKills
		// The newest bucket that reported wins the limits: a release that
		// changed them mid-window should be read against what it runs under
		// now, and the rows arrive oldest first.
		if limit := parseFloat(row["cpuLimit"]); limit > 0 {
			series.CPULimitCores = limit
		}
		if limit := parseUint(row["memoryLimit"]); limit > 0 {
			series.MemoryLimitBytes = limit
		}
	}
	return series, nil
}

// resourceSeriesStatement is the same shape over either source: per (pod,
// container) inside the bucket first, then across the environment.
//
// The two-level grouping is not a flourish. A container's usage over a bucket
// is its mean, and an environment's usage is the sum of its containers — doing
// it in one pass would average across pods and turn "three replicas at 100m"
// into 100m rather than 300m.
//
// `slot` is deliberately not called `bucket`: the outer SELECT renders the
// bucket as a string, and ClickHouse resolves a GROUP BY name against the
// SELECT aliases before the columns, so the grouping would be by the rendered
// string rather than by the time.
func resourceSeriesStatement(database string, rollup bool) string {
	inner := rawResourceSelect(database)
	if rollup {
		inner = rollupResourceSelect(database)
	}

	return fmt.Sprintf(`SELECT
    toString(toUnixTimestamp(slot)) AS bucket,
    toString(sum(cpu)) AS cpu,
    toString(sum(cpuPeak)) AS cpuPeak,
    toString(toUInt64(sum(mem))) AS memory,
    toString(toUInt64(sum(memPeak))) AS memoryPeak,
    toString(uniqExact(pod)) AS replicas,
    toString(toUInt64(sum(restarts))) AS restarts,
    toString(toUInt64(sum(oom))) AS oomKills,
    toString(max(cpuLimit)) AS cpuLimit,
    toString(toUInt64(max(memLimit))) AS memoryLimit
FROM (
    %s
)
GROUP BY slot
ORDER BY slot
FORMAT JSONEachRow`, inner)
}

// rawResourceSelect reads the metric tables directly, for a bucket finer than
// the rollup's.
//
// It is a UNION ALL because the numbers live in two tables: usage and limits
// are gauges, restarts and OOM kills are delta sums. Each branch contributes
// its own columns and zero for the other's, which the outer sum adds up to the
// same answer a single table would have given. `uniqExact(pod)` over the union
// counts a pod that reported either, which is the honest reading of "how many
// replicas were there".
//
// Within a branch the metric name does the demultiplexing: a metric table is
// one row per metric per scrape, so CPU and memory for one container are
// different rows and `avgIf(Value, MetricName = …)` is what makes them columns
// again.
func rawResourceSelect(database string) string {
	return fmt.Sprintf(`SELECT
        toStartOfInterval(g.TimeUnix, toIntervalSecond({width:UInt32})) AS slot,
        g.pod AS pod, g.container AS container,
        avgIf(g.Value, g.MetricName = %[3]s) AS cpu,
        maxIf(g.Value, g.MetricName = %[3]s) AS cpuPeak,
        avgIf(g.Value, g.MetricName = %[4]s) AS mem,
        maxIf(g.Value, g.MetricName = %[4]s) AS memPeak,
        toFloat64(0) AS restarts,
        toFloat64(0) AS oom,
        maxIf(g.Value, g.MetricName = %[5]s) AS cpuLimit,
        maxIf(g.Value, g.MetricName = %[6]s) AS memLimit
    FROM %[1]s.%[2]s AS g
    WHERE g.project = {project:String}
      AND g.environment = {environment:String}
      AND g.MetricName IN (%[3]s, %[4]s, %[5]s, %[6]s)
      AND g.TimeUnix >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND g.TimeUnix <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY slot, pod, container
    UNION ALL
    SELECT
        toStartOfInterval(s.TimeUnix, toIntervalSecond({width:UInt32})) AS slot,
        s.pod AS pod, s.container AS container,
        toFloat64(0) AS cpu,
        toFloat64(0) AS cpuPeak,
        toFloat64(0) AS mem,
        toFloat64(0) AS memPeak,
        sumIf(s.Value, s.MetricName = %[8]s) AS restarts,
        sumIf(s.Value, s.MetricName = %[9]s) AS oom,
        toFloat64(0) AS cpuLimit,
        toFloat64(0) AS memLimit
    FROM %[1]s.%[7]s AS s
    WHERE s.project = {project:String}
      AND s.environment = {environment:String}
      AND s.MetricName IN (%[8]s, %[9]s)
      AND s.TimeUnix >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND s.TimeUnix <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY slot, pod, container`,
		quoteIdentifier(database), quoteIdentifier(MetricsGaugeTable),
		quoteLiteral(MetricContainerCPUUsage), quoteLiteral(MetricContainerMemoryWorkingSet),
		quoteLiteral(MetricContainerCPULimit), quoteLiteral(MetricContainerMemoryLimit),
		quoteIdentifier(MetricsSumTable),
		quoteLiteral(MetricContainerRestartsDelta), quoteLiteral(MetricContainerOOMKilled))
}

// rollupResourceSelect reads the five-minute rollup, for anything wider.
//
// Every column is read through the table's alias for the reason
// createMetricsRollupGaugeView spells out: the state columns are named after
// the columns they aggregate.
func rollupResourceSelect(database string) string {
	return fmt.Sprintf(`SELECT
        toStartOfInterval(r.bucket, toIntervalSecond({width:UInt32})) AS slot,
        r.pod AS pod, r.container AS container,
        avgMerge(r.cpuCores) AS cpu,
        maxMerge(r.cpuPeakCores) AS cpuPeak,
        avgMerge(r.memoryBytes) AS mem,
        maxMerge(r.memoryPeakBytes) AS memPeak,
        sumMerge(r.restarted) AS restarts,
        sumMerge(r.oomKills) AS oom,
        maxMerge(r.cpuLimitCores) AS cpuLimit,
        maxMerge(r.memoryLimitBytes) AS memLimit
    FROM %s.%s AS r
    WHERE r.project = {project:String}
      AND r.environment = {environment:String}
      AND r.bucket >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND r.bucket <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY slot, pod, container`,
		quoteIdentifier(database), quoteIdentifier(MetricsRollupTable))
}

// resourceBucketSeconds picks the narrowest rung of the ladder that fits the
// span into the requested number of points.
func resourceBucketSeconds(span time.Duration, buckets int) int {
	wanted := int(span.Seconds()) / buckets
	for _, width := range resourceBucketLadder {
		if width >= wanted {
			return width
		}
	}
	return resourceBucketLadder[len(resourceBucketLadder)-1]
}

func parseFloat(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0
	}
	return parsed
}

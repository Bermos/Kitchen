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

package signals

import (
	"time"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The store's two series shapes, projected into [Bucket] so that the sustained
// and baseline machinery in window.go has one thing to read.
//
// The projections live here rather than in the rules because deciding what
// counts as an *observation* is a judgement about the source, not about the
// rule. A request bucket with no requests in it is an observation — the
// environment served nothing, and that is the fact env.traffic-vanished exists
// to notice. A resource bucket with no memory reading is not an observation:
// the environment idled to zero, or the collector missed a scrape, and neither
// means the container was using no memory.

// requestBuckets projects one field of a request series.
func requestBuckets(series clickhouse.RequestSeries, value func(clickhouse.RequestPoint) float64) []Bucket {
	buckets := make([]Bucket, 0, len(series.Points))
	for _, point := range series.Points {
		buckets = append(buckets, Bucket{
			Start: point.Start,
			Value: value(point),
			// Every bucket of a request series is real: the reader fills the
			// empty ones deliberately, because a gap in traffic is the most
			// interesting shape on the chart.
			Observed: true,
		})
	}
	return buckets
}

// ratioBuckets is requestBuckets for a field that is only defined where there
// was traffic. An error *rate* over zero requests is not zero, it is nothing,
// and averaging it in would drag every baseline towards the quiet hours.
func ratioBuckets(series clickhouse.RequestSeries, value func(clickhouse.RequestPoint) float64) []Bucket {
	buckets := make([]Bucket, 0, len(series.Points))
	for _, point := range series.Points {
		buckets = append(buckets, Bucket{
			Start:    point.Start,
			Value:    value(point),
			Observed: point.Requests > 0,
		})
	}
	return buckets
}

// requestWidth is one bucket of a request series, as a duration.
func requestWidth(series clickhouse.RequestSeries) time.Duration {
	return time.Duration(series.BucketSeconds) * time.Second
}

// resourceWidth is one bucket of a resource series, as a duration.
func resourceWidth(series clickhouse.ResourceSeries) time.Duration {
	return time.Duration(series.BucketSeconds) * time.Second
}

// memoryFractionBuckets is peak memory as a fraction of the limit.
//
// The peak rather than the mean, because the question is "did this ever come
// near the limit" and the kernel kills on an instant, not on an average. The
// limit comes from the series' own newest sample, so a release that raised it
// mid-window is judged against what it runs under now.
func memoryFractionBuckets(series clickhouse.ResourceSeries) []Bucket {
	limit := float64(series.MemoryLimitBytes)
	return resourceFractionBuckets(series, limit, func(point clickhouse.ResourcePoint) (float64, bool) {
		return float64(point.MemoryPeakBytes), point.MemoryPeakBytes > 0
	})
}

// cpuFractionBuckets is peak CPU as a fraction of the limit.
func cpuFractionBuckets(series clickhouse.ResourceSeries) []Bucket {
	return resourceFractionBuckets(series, series.CPULimitCores,
		func(point clickhouse.ResourcePoint) (float64, bool) {
			return point.CPUPeakCores, point.CPUPeakCores > 0
		})
}

// resourceFractionBuckets is the shared half of the two above: a usage series
// over a limit, with the buckets that reported nothing left unobserved.
//
// A zero or missing limit yields no series at all rather than a series of
// infinities. An environment whose release sets no limits cannot be near one,
// and saying so by producing nothing is what keeps the rule quiet about it.
func resourceFractionBuckets(
	series clickhouse.ResourceSeries,
	limit float64,
	usage func(clickhouse.ResourcePoint) (float64, bool),
) []Bucket {
	if limit <= 0 {
		return nil
	}
	buckets := make([]Bucket, 0, len(series.Points))
	for _, point := range series.Points {
		value, observed := usage(point)
		buckets = append(buckets, Bucket{
			Start:    point.Start,
			Value:    value / limit,
			Observed: observed,
		})
	}
	return buckets
}

// restartBuckets and oomBuckets are counts per bucket, which are observations
// wherever the bucket exists: zero restarts is a fact.
func restartBuckets(series clickhouse.ResourceSeries) []Bucket {
	return resourceCountBuckets(series, func(point clickhouse.ResourcePoint) float64 {
		return float64(point.Restarts)
	})
}

func oomBuckets(series clickhouse.ResourceSeries) []Bucket {
	return resourceCountBuckets(series, func(point clickhouse.ResourcePoint) float64 {
		return float64(point.OOMKills)
	})
}

func resourceCountBuckets(
	series clickhouse.ResourceSeries,
	value func(clickhouse.ResourcePoint) float64,
) []Bucket {
	buckets := make([]Bucket, 0, len(series.Points))
	for _, point := range series.Points {
		buckets = append(buckets, Bucket{Start: point.Start, Value: value(point), Observed: true})
	}
	return buckets
}

// bucketsSince keeps a series to the buckets at or after an instant. It is how
// a rule asks "in the last thirty minutes" of a series gathered over four
// hours.
func bucketsSince(buckets []Bucket, at time.Time) []Bucket {
	recent, _ := Split(buckets, at)
	return recent
}

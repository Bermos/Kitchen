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

import "time"

// What "sustained" and "against the trailing baseline" mean, defined once.
//
// Five rules in §7 need one or both — workload.near-memory-limit,
// workload.at-cpu-limit, node.saturated, env.error-rate, env.latency-regressed,
// platform.latency-correlated — and if each defined its own, the catalogue
// would quietly hold five different ideas of how long a problem has to last
// before it counts. Worse, none of them would be testable in isolation: the
// semantics would be entangled with the threshold and the data source in every
// rule. So the semantics live here, take no thresholds of their own, and have
// their own tests.

// Bucket is one time-bucketed observation.
//
// Observed is the field that earns its place. Every series these rules read
// ends in a partial bucket, and an environment that scaled to zero stops
// producing buckets entirely — so "the value is 0" and "there was no value"
// have to be different things. Treating an absent bucket as a zero is how a
// memory-limit rule stops firing the moment the pod that is about to be killed
// gets slow enough to miss a scrape.
type Bucket struct {
	Start    time.Time
	Value    float64
	Observed bool
}

// Run is the trailing stretch of a series in which a condition held: the
// newest buckets, walked backwards until the condition stopped holding.
type Run struct {
	// Since is the start of the oldest bucket in the run, and is what a
	// finding reports as the moment the condition began.
	Since time.Time
	// Buckets is how many observations the run covers.
	Buckets int
	// Duration is from Since to the end of the newest bucket in the run.
	Duration time.Duration
	// Peak and Latest are the run's worst and most recent values, which is
	// what a finding's detail quotes.
	Peak   float64
	Latest float64
}

// Sustained is the one definition. A condition is sustained when it held
// through the newest end of the series for at least [SustainedWindow], across
// at least [MinSustainedBuckets] observations.
//
// Both halves are load-bearing. Without the duration, three buckets of a
// coarse series could span two minutes; without the count, a single sample in
// a wide bucket could claim to cover an hour.
func (r Run) Sustained() bool {
	return r.Duration >= SustainedWindow && r.Buckets >= MinSustainedBuckets
}

// TrailingRun measures how far back from the newest end of a series a
// condition held.
//
// The series must be ordered oldest first, which is how every reader in
// internal/clickhouse returns one. `width` is one bucket's width, carried
// separately because a series knows it and the buckets do not — and because
// the last bucket's duration cannot be inferred from the gap to a bucket that
// does not exist yet.
//
// `now` is the snapshot's instant, not the wall clock. It decides whether the
// run reaches the present: a run that ended more than [MaxTrailingGap] ago is
// not a current condition, however long it lasted, and this returns the zero
// Run for it.
func TrailingRun(series []Bucket, width time.Duration, now time.Time, holds func(float64) bool) Run {
	if width <= 0 {
		return Run{}
	}

	// Walk back over the trailing buckets that carry no observation at all.
	// The newest bucket of any series is partial and often empty, and refusing
	// to look past it would mean no condition was ever sustained. Past
	// MaxTrailingGap of silence the series has stopped rather than paused, and
	// whatever is behind the gap is history.
	end := len(series)
	for end > 0 && !series[end-1].Observed {
		end--
	}
	if end == 0 {
		return Run{}
	}
	newest := series[end-1]
	if now.Sub(newest.Start.Add(width)) > MaxTrailingGap {
		return Run{}
	}

	run := Run{Latest: newest.Value}
	for i := end - 1; i >= 0; i-- {
		bucket := series[i]
		// A gap in the middle breaks the run. The condition may well have held
		// across it, but the series cannot say so, and a rule that guessed
		// would be reporting a duration it did not measure.
		if !bucket.Observed || !holds(bucket.Value) {
			break
		}
		run.Since = bucket.Start
		run.Buckets++
		if bucket.Value > run.Peak {
			run.Peak = bucket.Value
		}
	}
	if run.Buckets == 0 {
		return Run{}
	}
	run.Duration = newest.Start.Add(width).Sub(run.Since)
	return run
}

// Regression is a recent measurement beside the trailing baseline it is
// supposed to resemble.
type Regression struct {
	Recent   float64
	Baseline float64
	// Support is how many observations the baseline was computed from. A
	// baseline with too little behind it is not a baseline, and comparing
	// against one is how a freshly deployed environment reports that it has
	// regressed against the four minutes before it existed.
	Support int
}

// Regressed reports whether Recent has risen above Baseline by at least
// `factor`, with `floor` as the value below which the ratio is not worth
// acting on.
//
// The floor is why this is not a one-line comparison. A p95 that moves from
// 4ms to 12ms has tripled and means nothing; an error rate that moves from
// 0.05% to 0.2% has quadrupled and means nothing. Every ratio rule in the
// catalogue needs the same guard, so it lives with the ratio.
func (r Regression) Regressed(factor, floor float64) bool {
	switch {
	case r.Support < MinBaselineBuckets:
		return false
	case r.Recent < floor:
		return false
	case r.Baseline <= 0:
		// Nothing to divide by. Clearing the floor from a baseline of nothing
		// is a change worth reporting — that is the shape of an error rate
		// appearing where there was none.
		return true
	}
	return r.Recent >= r.Baseline*factor
}

// Elevated is Regressed's other half, for a measurement that is wrong on its
// own terms and that a baseline may excuse rather than establish.
//
// The difference matters at the edges and only there. An error rate of 20% is
// worth reporting whether or not there is a baseline to compare it against —
// an environment that has failed one request in five since it was created has
// not "regressed", it has never worked — so an unestablished baseline lets it
// through here where it suppresses in [Regression.Regressed]. A latency
// regression has no such absolute form: nobody can say what p95 is too slow for
// an application they have never seen, which is exactly why that rule needs a
// baseline and this one does not.
func (r Regression) Elevated(factor, floor float64) bool {
	switch {
	case r.Recent < floor:
		return false
	case r.Support < MinBaselineBuckets, r.Baseline <= 0:
		return true
	}
	return r.Recent >= r.Baseline*factor
}

// Split divides a series at an instant: the buckets from `at` onwards are the
// recent window, everything before them the trailing baseline.
//
// One read covers both, which is not only cheaper than two queries but more
// honest: the two windows come from the same rollup, at the same resolution,
// and cannot disagree about where one ends and the other begins.
func Split(series []Bucket, at time.Time) (recent, baseline []Bucket) {
	for i := range series {
		if !series[i].Start.Before(at) {
			return series[i:], series[:i]
		}
	}
	return nil, series
}

// Mean averages the observed buckets of a series and reports how many there
// were. Unobserved buckets are skipped rather than counted as zero, for the
// reason [Bucket.Observed] exists.
func Mean(series []Bucket) (float64, int) {
	var total float64
	var observed int
	for _, bucket := range series {
		if !bucket.Observed {
			continue
		}
		total += bucket.Value
		observed++
	}
	if observed == 0 {
		return 0, 0
	}
	return total / float64(observed), observed
}

// Sum totals the observed buckets of a series.
func Sum(series []Bucket) float64 {
	var total float64
	for _, bucket := range series {
		if bucket.Observed {
			total += bucket.Value
		}
	}
	return total
}

// Slope fits a straight line through the observed buckets by least squares and
// returns the change in value per second, plus how many points it fitted.
//
// It is the whole of node.disk-filling's "projected full within N days". A
// least-squares fit rather than a first-to-last difference because a
// filesystem's fill is noisy — a log rotation or a merge can undo an hour of
// growth — and two endpoints chosen either side of one of those predict a date
// that is off by weeks in either direction.
func Slope(series []Bucket) (perSecond float64, points int) {
	var n, sumX, sumY, sumXY, sumXX float64
	var origin time.Time
	for _, bucket := range series {
		if !bucket.Observed {
			continue
		}
		if n == 0 {
			origin = bucket.Start
		}
		x := bucket.Start.Sub(origin).Seconds()
		n++
		sumX += x
		sumY += bucket.Value
		sumXY += x * bucket.Value
		sumXX += x * x
	}
	if n < 2 {
		return 0, int(n)
	}
	denominator := n*sumXX - sumX*sumX
	if denominator == 0 {
		// Every observation landed at the same instant, which a bucketed
		// series can only manage with one bucket repeated. There is no line
		// through that.
		return 0, int(n)
	}
	return (n*sumXY - sumX*sumY) / denominator, int(n)
}

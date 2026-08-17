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
	"math"
	"testing"
	"time"
)

// "Sustained" is defined once, so it is tested once — here, against the shapes
// that would otherwise each have to be discovered separately in five rules.

// window builds a series of `count` buckets ending at testNow, each carrying
// the value `at` returns. A negative value marks a bucket with no observation,
// which is the case the whole Observed field exists for.
func window(width time.Duration, count int, at func(i int) float64) []Bucket {
	start := testNow.Add(-time.Duration(count) * width)
	buckets := make([]Bucket, count)
	for i := range buckets {
		value := at(i)
		buckets[i] = Bucket{
			Start:    start.Add(time.Duration(i) * width),
			Value:    math.Abs(value),
			Observed: value >= 0,
		}
	}
	return buckets
}

func above(threshold float64) func(float64) bool {
	return func(value float64) bool { return value >= threshold }
}

func TestTrailingRunSustainedNeedsBothDurationAndCount(t *testing.T) {
	for name, testCase := range map[string]struct {
		width     time.Duration
		count     int
		at        func(i int) float64
		sustained bool
		buckets   int
	}{
		"held throughout": {
			width: time.Minute, count: 20,
			at:        func(int) float64 { return 0.95 },
			sustained: true, buckets: 20,
		},
		"long enough but too few buckets": {
			// Two buckets spanning half an hour look sustained by the clock
			// and show no trend at all, which is what MinSustainedBuckets is
			// for.
			width: 15 * time.Minute, count: 2,
			at:        func(int) float64 { return 0.95 },
			sustained: false, buckets: 2,
		},
		"enough buckets but too short": {
			width: time.Minute, count: 4,
			at:        func(int) float64 { return 0.95 },
			sustained: false, buckets: 4,
		},
		"a spike at the end": {
			width: time.Minute, count: 20,
			at:        func(i int) float64 { return map[bool]float64{true: 0.95, false: 0.10}[i >= 19] },
			sustained: false, buckets: 1,
		},
		"recovered": {
			width: time.Minute, count: 20,
			at:        func(i int) float64 { return map[bool]float64{true: 0.10, false: 0.95}[i >= 15] },
			sustained: false, buckets: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := TrailingRun(window(testCase.width, testCase.count, testCase.at),
				testCase.width, testNow, above(0.90))
			if run.Sustained() != testCase.sustained {
				t.Fatalf("sustained = %v, want %v (run %+v)", run.Sustained(), testCase.sustained, run)
			}
			if run.Buckets != testCase.buckets {
				t.Fatalf("buckets = %d, want %d", run.Buckets, testCase.buckets)
			}
		})
	}
}

// The newest bucket of any series is partial and often empty. A run that
// refused to look past it would never be sustained.
func TestTrailingRunLooksPastAnEmptyNewestBucket(t *testing.T) {
	buckets := window(time.Minute, 20, func(i int) float64 {
		if i == 19 {
			return -1
		}
		return 0.95
	})
	run := TrailingRun(buckets, time.Minute, testNow, above(0.90))
	if !run.Sustained() {
		t.Fatalf("a trailing partial bucket broke the run: %+v", run)
	}
}

// Silence past MaxTrailingGap is a series that stopped, and whatever is behind
// it is history rather than a current condition.
func TestTrailingRunIgnoresAStaleSeries(t *testing.T) {
	width := time.Minute
	buckets := window(width, 20, func(int) float64 { return 0.95 })
	stale := testNow.Add(MaxTrailingGap + width + time.Minute)
	if run := TrailingRun(buckets, width, stale, above(0.90)); run.Buckets != 0 {
		t.Fatalf("a series that stopped %s ago still reported a run: %+v", MaxTrailingGap, run)
	}
}

// A gap in the middle breaks the run: the condition may well have held across
// it, but the series cannot say so.
func TestTrailingRunBreaksOnAGap(t *testing.T) {
	buckets := window(time.Minute, 20, func(i int) float64 {
		if i == 15 {
			return -1
		}
		return 0.95
	})
	run := TrailingRun(buckets, time.Minute, testNow, above(0.90))
	if run.Buckets != 4 {
		t.Fatalf("buckets = %d, want the four after the gap", run.Buckets)
	}
}

func TestTrailingRunReportsWhenTheConditionStarted(t *testing.T) {
	width := time.Minute
	buckets := window(width, 20, func(i int) float64 {
		if i < 10 {
			return 0.10
		}
		return 0.95
	})
	// A different threshold, to prove the run is measured against the
	// predicate it was given rather than against anything baked in.
	run := TrailingRun(buckets, width, testNow, above(0.50))
	want := testNow.Add(-10 * width)
	if !run.Since.Equal(want) {
		t.Fatalf("since = %s, want %s", run.Since, want)
	}
	if run.Duration != 10*width {
		t.Fatalf("duration = %s, want %s", run.Duration, 10*width)
	}
}

func TestRegressedNeedsABaselineAndAFloor(t *testing.T) {
	for name, testCase := range map[string]struct {
		comparison Regression
		want       bool
	}{
		"doubled over a real baseline": {
			comparison: Regression{Recent: 900, Baseline: 400, Support: 40}, want: true,
		},
		"doubled but too small to matter": {
			// 12ms from 5ms is a regression by ratio and nobody's incident.
			comparison: Regression{Recent: 12, Baseline: 5, Support: 40}, want: false,
		},
		"no baseline to compare against": {
			// A freshly created environment has not regressed against the four
			// minutes before it existed.
			comparison: Regression{Recent: 900, Baseline: 400, Support: 2}, want: false,
		},
		"a baseline of nothing": {
			comparison: Regression{Recent: 900, Baseline: 0, Support: 40}, want: true,
		},
		"unchanged": {
			comparison: Regression{Recent: 420, Baseline: 400, Support: 40}, want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := testCase.comparison.Regressed(LatencyRegressionFactor, LatencyFloorMs)
			if got != testCase.want {
				t.Fatalf("regressed = %v, want %v", got, testCase.want)
			}
		})
	}
}

// Elevated differs from Regressed at exactly one place, and it is the place
// that matters: an absolute threshold is worth reporting whether or not there
// is a baseline behind it.
func TestElevatedFiresWithoutAnEstablishedBaseline(t *testing.T) {
	comparison := Regression{Recent: 0.5, Baseline: 0, Support: 0}
	if !comparison.Elevated(ErrorRateFactor, ErrorRateFiring) {
		t.Fatal("half the requests failing since birth should be elevated")
	}
	if comparison.Regressed(ErrorRateFactor, ErrorRateFiring) {
		t.Fatal("the same measurement should not count as a regression")
	}
}

func TestElevatedIsExcusedByAServicesOwnNormal(t *testing.T) {
	// An application that always answers 6% errors is not news every fifteen
	// minutes forever.
	comparison := Regression{Recent: 0.06, Baseline: 0.055, Support: 40}
	if comparison.Elevated(ErrorRateFactor, ErrorRateFiring) {
		t.Fatal("a rate matching its own baseline should be excused")
	}
}

func TestSplitDividesAtTheInstant(t *testing.T) {
	buckets := window(time.Minute, 20, func(int) float64 { return 1 })
	recent, baseline := Split(buckets, testNow.Add(-5*time.Minute))
	if len(recent) != 5 || len(baseline) != 15 {
		t.Fatalf("split %d/%d, want 5/15", len(recent), len(baseline))
	}
}

func TestMeanSkipsUnobservedBuckets(t *testing.T) {
	buckets := window(time.Minute, 4, func(i int) float64 {
		if i%2 == 0 {
			return -1
		}
		return 10
	})
	mean, observed := Mean(buckets)
	if mean != 10 || observed != 2 {
		t.Fatalf("mean = %v over %d observations, want 10 over 2", mean, observed)
	}
}

func TestSlopeFitsGrowth(t *testing.T) {
	// A tenth of the volume per bucket of five minutes.
	buckets := window(5*time.Minute, 10, func(i int) float64 { return float64(i) / 10 })
	perSecond, points := Slope(buckets)
	if points != 10 {
		t.Fatalf("fitted %d points, want 10", points)
	}
	want := 0.1 / (5 * 60)
	if math.Abs(perSecond-want) > want/100 {
		t.Fatalf("slope = %v per second, want about %v", perSecond, want)
	}
}

func TestSlopeRefusesASinglePoint(t *testing.T) {
	if _, points := Slope(window(time.Minute, 1, func(int) float64 { return 1 })); points != 1 {
		t.Fatalf("fitted %d points, want 1", points)
	}
	if perSecond, _ := Slope(window(time.Minute, 1, func(int) float64 { return 1 })); perSecond != 0 {
		t.Fatalf("slope through one point = %v, want 0", perSecond)
	}
}

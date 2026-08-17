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

// Every number the catalogue compares against, in one file, each with the
// reason it is that number.
//
// They are constants rather than configuration, which docs/OBSERVABILITY.md §7
// settles explicitly: configurable thresholds are an alerting-era feature, and
// the catalogue is versioned code either way. They are gathered here rather
// than spread across the rules because the only way to keep thirty-six rules
// agreeing on what "sustained" or "near the limit" means is to make disagreeing
// require editing the same screenful of text.
//
// The taste behind them, since it is not visible in any single number: a rule
// fires when a competent operator woken by it would agree they wanted waking.
// That biases every threshold towards silence — a spike is not an incident, a
// short window is not a trend, and a number without enough observations behind
// it is not a number.

// The windows every series-reading rule shares.
const (
	// RecentWindow is what "now" means. Fifteen minutes is long enough that a
	// deploy's restart, a single slow request or one scrape gap does not fill
	// it, and short enough that a problem that started twenty minutes ago is
	// still described as current.
	RecentWindow = 15 * time.Minute

	// BaselineWindow is the trailing stretch a recent window is compared
	// against, ending where the recent window begins. Four hours spans a
	// working morning: long enough that one bad deploy does not become the
	// baseline, short enough that yesterday's traffic shape does not.
	BaselineWindow = 4 * time.Hour

	// SustainedWindow is how long a condition must hold to count as sustained.
	// Ten minutes is two Kubernetes probe cycles and two metric rollup buckets
	// — past the point where a garbage collection pause, a rolling update or a
	// cold cache explains it.
	SustainedWindow = 10 * time.Minute

	// MinSustainedBuckets is how many observations a sustained run needs
	// regardless of how long it looks. Three is the smallest number that can
	// show a trend rather than an edge: with two, one bad sample at each end is
	// indistinguishable from a step change.
	MinSustainedBuckets = 3

	// MaxTrailingGap is how much silence at the newest end of a series is
	// forgiven before a run counts as stale rather than current. A series
	// always ends in a partial bucket, and an environment that idled to zero
	// stops reporting entirely; ten minutes tolerates both without letting a
	// condition from an hour ago be described in the present tense.
	MaxTrailingGap = 10 * time.Minute

	// MinBaselineBuckets is how many observations a baseline needs before
	// anything is compared against it. Ten is roughly the first two and a half
	// minutes of a fresh environment's life at the finest bucket width, and
	// comparing against less than that is how a deploy becomes a "regression".
	MinBaselineBuckets = 10
)

// The resolutions the two series are gathered at. They are here rather than in
// the gatherer because they are the other half of the sustained definition: a
// series bucketed more coarsely than SustainedWindow/MinSustainedBuckets can
// never satisfy it, however long the condition lasts.
const (
	// TrafficWindow is how much of the request rollup one gather reads: the
	// recent window and the baseline it is compared against, in one query.
	TrafficWindow = RecentWindow + BaselineWindow

	// TrafficBucket is the resolution the request series is asked for. Two
	// minutes is the finest rung of the reader's own ladder at or below
	// SustainedWindow/MinSustainedBuckets, which is what makes five buckets of
	// it add up to exactly the ten minutes "sustained" means.
	TrafficBucket = 2 * time.Minute

	// ResourceWindow is how much of the metric rollup one gather reads. An
	// hour covers OOMWindow, which is the longest look-back any resource rule
	// takes.
	ResourceWindow = time.Hour

	// ResourceBucket is the resolution the resource series is asked for, and
	// is the rollup's own: at five minutes the reader answers from metrics_5m
	// rather than from the raw metric tables, which is sixty times less to
	// read for the same numbers. Three of them span fifteen minutes, so a
	// sustained run here is stricter than SustainedWindow asks and never
	// looser.
	ResourceBucket = 5 * time.Minute
)

// Workloads.
const (
	// RestartWindow is how far back restarts are counted. Thirty minutes is
	// the window docs/OBSERVABILITY.md §6.1 renders — "12 restarts in 30m" — and
	// it is long enough to survive CrashLoopBackOff's own back-off, which
	// stretches to five minutes between attempts and would make a shorter
	// window report a settled crash loop as calm.
	RestartWindow = 30 * time.Minute

	// RestartsFiring is the restart count in that window that means something
	// is wrong rather than something was deployed. A rollout costs one restart
	// per container; five is past any explanation but a loop.
	RestartsFiring = 5

	// OOMWindow is how far back OOM kills are counted. An hour, because a
	// single OOM kill is worth reporting for longer than a restart is: it
	// names its own cause, and the fix is a number in a manifest.
	OOMWindow = time.Hour

	// MemoryLimitFraction is where "near the limit" starts. The kernel kills at
	// 100% with no warning and no grace, so the margin has to be wide enough to
	// act in: at 90% of a 512Mi limit there are 50Mi left, which one request
	// can consume.
	MemoryLimitFraction = 0.90

	// CPULimitFraction is where CPU counts as pinned. It is higher than the
	// memory fraction because the consequence is milder — throttling, not
	// death — and because a container legitimately using most of its CPU quota
	// is a container sized correctly.
	CPULimitFraction = 0.95

	// NotReadyGrace is how long a workload may have fewer pods available than
	// it wants before that is a problem rather than a rollout. Ten minutes
	// clears an image pull on a cold node and a readiness probe's initial
	// delay.
	NotReadyGrace = 10 * time.Minute

	// PendingGrace is how long a pod may sit unschedulable before it is
	// reported. Two minutes is past the cluster autoscaler's reaction time on
	// installations that have one, and well past the scheduler's own retries.
	PendingGrace = 2 * time.Minute
)

// Environment traffic, read from the request rollup.
const (
	// ErrorRateFiring is the 5xx share that is wrong on its own terms,
	// whatever the baseline was. Five percent means one request in twenty is
	// failing, which no healthy service does for fifteen minutes.
	ErrorRateFiring = 0.05

	// ErrorRateFactor is how much worse than the trailing baseline a rate has
	// to be to count as a regression rather than as this service's normal.
	// Three times, because an application that always answers 1% errors should
	// not be reported forever, but one that goes from 1% to 3% has changed.
	ErrorRateFactor = 3.0

	// MinRequestsToJudge is how much traffic a window needs before a ratio
	// computed from it means anything. Twenty requests is the point below
	// which one failure is 5% — the firing threshold — by arithmetic rather
	// than by fault.
	MinRequestsToJudge = 20

	// LatencyRegressionFactor is how much slower than baseline p95 counts as a
	// regression. Doubling is a number a person recognises as "something
	// changed"; 20% is noise at p95 with realistic traffic.
	LatencyRegressionFactor = 2.0

	// LatencyFloorMs is the p95 below which a doubling is not worth reporting.
	// Going from 5ms to 15ms is a tripling and is nobody's incident.
	LatencyFloorMs = 250.0

	// TrafficVanishedBaselineRPS is how much traffic the baseline must have
	// had before its disappearance is news. A twentieth of a request per second
	// is about three requests a minute — below that, an idle preview looks
	// identical to a broken one.
	TrafficVanishedBaselineRPS = 0.05

	// NoBackendMinErrors is how many 5xx the edge must have served while
	// nothing was ready before that combination is reported. Five, because one
	// request landing during the second between the last pod going and the new
	// one arriving is a rollout.
	NoBackendMinErrors = 5
)

// Nodes and capacity.
const (
	// NodeSaturationFraction is where a node counts as saturated. Ninety
	// percent sustained is where scheduling headroom is gone and latency starts
	// coming from the node rather than from the application — which is exactly
	// what platform.latency-correlated links to as its explanation.
	NodeSaturationFraction = 0.90

	// DiskFillDays is how near a projected full disk has to be to report. Seven
	// days is a week's notice: enough to order a disk, past the point where a
	// slow leak is indistinguishable from noise.
	DiskFillDays = 7

	// DiskFillMinFraction is how full a filesystem must already be before a
	// projection through it is taken seriously. A straight line through a disk
	// at 4% predicts a date that means nothing; at half full, the slope is
	// measuring something real.
	DiskFillMinFraction = 0.50

	// NodeSilentAfter is how long a node may report nothing before its
	// collector is presumed dead. Ten minutes, as §7 specifies: the collector
	// scrapes on a much shorter interval, so silence this long is not a missed
	// scrape.
	NodeSilentAfter = 10 * time.Minute

	// FreshnessLookback is how far back the freshness query looks. An hour
	// bounds the query to a day's partitions while leaving room to distinguish
	// "silent for twelve minutes" from "silent since the install".
	FreshnessLookback = time.Hour

	// OvercommitHeadroomNodes is how many nodes the cluster must be able to
	// lose and still fit its pods. One: the claim being made is "the next node
	// loss cannot be rescheduled", and that is a claim about one node.
	OvercommitHeadroomNodes = 1
)

// Storage.
const (
	// VolumeFullFraction is where a claim counts as filling. Eighty-five
	// percent leaves a database room to compact and a log volume room for the
	// burst that usually causes the last 15%.
	VolumeFullFraction = 0.85

	// StoreDiskFraction is the same judgement for the telemetry store's own
	// volume. It is the same number deliberately: the store is a database on a
	// claim like any other, and a second threshold would only be a second thing
	// to explain.
	StoreDiskFraction = 0.85

	// IngestStalledAfter is how old the newest row may be while pods are
	// running before ingest counts as stalled. Ten minutes matches
	// NodeSilentAfter, because the two rules describe the same silence from
	// opposite ends — one node's, or everyone's.
	IngestStalledAfter = 10 * time.Minute

	// FlowsLostFiring is how many flow events Relay may report dropping before
	// the request numbers are described as under-reporting. A hundred in the
	// accounting window is well past a momentary buffer overrun and is still
	// invisible in any total — which is the point: the numbers look fine, and
	// this is the only thing that says they are not.
	FlowsLostFiring = 100
)

// Edge and certificates.
const (
	// CertExpiryWindow is how close to expiry a certificate is reported when
	// its renewal is not progressing. Twenty-one days, per §7: cert-manager
	// renews at two thirds of a 90-day Let's Encrypt lifetime, so a certificate
	// still unrenewed inside three weeks has already failed at least one
	// attempt.
	CertExpiryWindow = 21 * 24 * time.Hour

	// UnroutedWindow is how far back unrouted hosts are counted, and
	// UnroutedMinRequests how many requests one host needs in it. A scanner
	// walking the address space asks once; a stale DNS record still carrying
	// real users asks continuously.
	UnroutedWindow      = time.Hour
	UnroutedMinRequests = 100

	// UnroutedSustained is how long a host must have kept asking. Thirty
	// minutes separates a burst of scanning from a name that is genuinely still
	// pointed here.
	UnroutedSustained = 30 * time.Minute

	// TunnelRestartsFiring is how many restarts make cloudflared "flapping"
	// rather than "restarted". Three in the restart window is a tunnel that
	// cannot hold a connection.
	TunnelRestartsFiring = 3

	// DNSProbeLimit is how many published names one evaluation resolves. Five
	// keeps the probe cheap — it happens on every platform-screen load — and
	// the failure it is looking for is systemic: if the wildcard record is
	// wrong, the first name proves it.
	DNSProbeLimit = 5
)

// Builds.
const (
	// BuildQueueFactor is how many median build times a build may wait before
	// the queue is backed up, and BuildQueueFloor the wait below which no
	// multiple counts. The floor matters most on a fresh platform, where the
	// median is zero and every queued build would otherwise look stuck.
	BuildQueueFactor = 3.0
	BuildQueueFloor  = 5 * time.Minute

	// BuildFailureStreak is how many consecutive failures in one project mean
	// the project is broken rather than a commit was. Three, because two is a
	// bad afternoon and the third is a pattern.
	BuildFailureStreak = 3

	// BuildLookback is how far back the build rules read. A day covers a
	// working cycle without making a project that was broken last week look
	// broken now.
	BuildLookback = 24 * time.Hour
)

// Cross-project detectors.
const (
	// CorrelatedProjects is how many projects must degrade together before it
	// is called a platform problem. Three, per §7: two projects sharing a node
	// or a dependency is a coincidence worth nothing, and three at once has
	// never been three unrelated causes.
	CorrelatedProjects = 3
)

// MaxDetailLength bounds one finding's detail so that a pathological Kubernetes
// message cannot dominate a screen. It matches the component survey's own bound
// for the same reason: the cause is at the front of these strings, so the tail
// is what can be lost.
const MaxDetailLength = 400

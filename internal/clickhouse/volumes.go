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
	"time"
)

// How full each mounted claim is.
//
// Nothing in the API server knows this. A PersistentVolumeClaim reports the
// size it was granted and never how much of it is left, so a volume filling up
// is invisible until the application writing to it starts failing — which is
// the whole reason the collector's kubelet scraper has the volume metric group
// turned on.

// The kubelet's volume metrics, as the `kubelet_stats` receiver emits them.
//
// There is deliberately no used-bytes metric here: the receiver has none at
// all — asking for `k8s.volume.used` is refused at startup as an unknown metric
// — so used is `capacity - available`, which is also the arithmetic the kubelet
// summary these are read from does.
const (
	MetricVolumeCapacity  = "k8s.volume.capacity"
	MetricVolumeAvailable = "k8s.volume.available"
)

// VolumeClaimAttribute is the resource attribute naming the claim a volume is
// bound to, and it is the one attribute this read cannot do without.
//
// It exists only because the chart asks the receiver for an extra metadata
// label (`k8s.volume.type`), which is what makes it read the kubelet's /pods
// endpoint beside /stats/summary. The summary names a volume the way the pod
// spec does — `data` — which is not the claim behind it, so without that read a
// full volume can be seen and not named. Volumes that are not claims —
// configMap, secret, the projected service-account token every pod carries —
// have no such attribute at all, which is what filters them out here.
//
// Verified against the collector image the chart pins (contrib 0.158.0), with
// the chart's own receiver configuration: the PVC-backed volume's rows carried
// this key and the projected one's did not.
const VolumeClaimAttribute = "k8s.persistentvolumeclaim.name"

// DefaultVolumeLookback is how far back a volume read looks for the newest
// sample of each claim.
//
// It bounds the scan, and it is also the definition of "still reporting": the
// kubelet is scraped every 30 seconds by default, so a claim whose newest
// sample is older than this is one whose node has gone quiet. That is
// `node.silent`'s subject, not a volume's, and answering with a stale fill
// figure would be the wrong signal wearing the right one's clothes.
const DefaultVolumeLookback = 15 * time.Minute

// DefaultVolumeLimit caps how many claims one read answers with.
const DefaultVolumeLimit = 500

// VolumeUsageQuery asks how full the platform's claims are.
type VolumeUsageQuery struct {
	// At is the instant the answer is about. Zero means now.
	At time.Time
	// Within is how far back to look for each claim's newest sample. Zero takes
	// DefaultVolumeLookback.
	Within time.Duration
	// Limit caps the rows. The fullest come first, so a cap can only ever drop
	// claims with room to spare.
	Limit int
}

// VolumeUsage is how full one PersistentVolumeClaim is.
//
// Project and Environment are what the collector's k8s_attributes processor
// stamped on the pod that mounts it, and are empty for a claim outside an
// application's namespace — the telemetry store's own, say. This package does
// not map a namespace back to a project: that is the operator's vocabulary, and
// the caller that has it joins them.
type VolumeUsage struct {
	Namespace   string `json:"namespace"`
	Claim       string `json:"claim"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Pod is where the sample came from. A claim mounted by several pods is
	// still one volume with one fill, so this names whichever pod reported it
	// most recently rather than multiplying the claim by its mounts.
	Pod           string  `json:"pod"`
	CapacityBytes uint64  `json:"capacityBytes"`
	UsedBytes     uint64  `json:"usedBytes"`
	UsedFraction  float64 `json:"usedFraction"`
}

// VolumeUsage answers how full every claim on the platform is, as of the newest
// sample each one has.
//
// One row per claim, not per mount: the two metrics are gauges taken per pod
// per volume, so a claim mounted by three pods reports three identical fills
// and a caller that fanned those out would raise the same warning three times.
func (c *Client) VolumeUsage(ctx context.Context, query VolumeUsageQuery) ([]VolumeUsage, error) {
	at := query.At
	if at.IsZero() {
		at = time.Now()
	}
	within := query.Within
	if within <= 0 {
		within = DefaultVolumeLookback
	}
	limit := query.Limit
	if limit < 1 {
		limit = DefaultVolumeLimit
	}
	at = at.UTC()

	params := map[string]string{
		"since": at.Add(-within).Format(time.RFC3339Nano),
		"until": at.Format(time.RFC3339Nano),
		"limit": strconv.Itoa(limit),
	}
	rows, err := c.selectionRows(ctx, volumeUsageStatement(c.cfg.Database), params)
	if err != nil {
		return nil, err
	}

	volumes := make([]VolumeUsage, 0, len(rows))
	for _, row := range rows {
		capacity := parseFloat(row["capacity"])
		available := parseFloat(row["available"])
		if capacity <= 0 {
			// A volume whose capacity has not been reported is not a volume at
			// 100%; it is a volume nothing has measured.
			continue
		}
		// A source that reports more free space than the volume has, or less
		// than none, has contradicted itself. An empty volume and a full one are
		// the two honest readings of that; a negative one and a 110% one are not
		// numbers anything downstream can render.
		used := capacity - available
		switch {
		case used < 0:
			used = 0
		case used > capacity:
			used = capacity
		}
		volumes = append(volumes, VolumeUsage{
			Namespace:     row["namespace"],
			Claim:         row["claim"],
			Project:       row["project"],
			Environment:   row["environment"],
			Pod:           row["pod"],
			CapacityBytes: uint64(capacity),
			UsedBytes:     uint64(used),
			UsedFraction:  used / capacity,
		})
	}
	return volumes, nil
}

// volumeUsageStatement is the newest capacity and available bytes per claim.
//
// `argMax` is doing the work: the two numbers are separate rows of the same
// scrape, so each is picked at the newest sample that carried it, and the pod
// beside them is the one that reported last. Reading a `max()` of each instead
// would take the capacity from one scrape and the free space from another,
// which is how a volume that has just been grown reads as full.
//
// Every column is read through the table's alias, which is not a style choice:
// the outer SELECT names its results after the same things — `project`,
// `namespace`, `pod` are all real columns — and ClickHouse resolves a bare name
// against the SELECT list's own aliases first. See createMetricsRollupGaugeView
// for what that shadowing costs where it is not caught.
func volumeUsageStatement(database string) string {
	return fmt.Sprintf(`SELECT
    namespace,
    claim,
    project,
    environment,
    pod,
    toString(capacityBytes) AS capacity,
    toString(availableBytes) AS available
FROM (
    SELECT
        v.namespace AS namespace,
        v.ResourceAttributes[%[5]s] AS claim,
        argMax(v.project, v.TimeUnix) AS project,
        argMax(v.environment, v.TimeUnix) AS environment,
        argMax(v.pod, v.TimeUnix) AS pod,
        argMaxIf(v.Value, v.TimeUnix, v.MetricName = %[3]s) AS capacityBytes,
        argMaxIf(v.Value, v.TimeUnix, v.MetricName = %[4]s) AS availableBytes
    FROM %[1]s.%[2]s AS v
    WHERE v.MetricName IN (%[3]s, %[4]s)
      AND v.ResourceAttributes[%[5]s] != ''
      AND v.TimeUnix >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND v.TimeUnix <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY namespace, claim
)
ORDER BY if(capacityBytes > 0, (capacityBytes - availableBytes) / capacityBytes, 0) DESC,
    namespace, claim
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		quoteIdentifier(database), quoteIdentifier(MetricsGaugeTable),
		quoteLiteral(MetricVolumeCapacity), quoteLiteral(MetricVolumeAvailable),
		quoteLiteral(VolumeClaimAttribute))
}

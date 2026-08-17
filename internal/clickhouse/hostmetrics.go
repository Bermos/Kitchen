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
	"sort"
	"strconv"
	"time"
)

// The machine underneath: what a node itself was doing, rather than what was
// scheduled on it.
//
// Every other read in this package is about an application. This one is about
// the hardware every application on a node shares and none of them can see: a
// node pinned at 90% CPU makes every workload on it slow, and nothing in the
// API server says so. The collector's `host_metrics` scrapers have been
// shipping these all along (see the chart's collector ConfigMap); this is what
// reads them back.
//
// Nothing here is derived from the metrics rollup. `metrics_5m` is fed by two
// materialized views that filter on the container metric names, so a node's own
// samples are not in it and never were.

// The host metrics this reads, as the `host_metrics` scrapers emit them, and
// the attribute their breakdown is carried in.
//
// These are the *enabled-by-default* metrics of the cpu, memory and filesystem
// scrapers. The obvious ones — `system.cpu.utilization`,
// `system.memory.utilization`, `system.filesystem.utilization` — are all
// disabled by default, so a reader that asked for them would find nothing and
// report a healthy cluster forever. The utilisations below are computed from
// the totals instead, which is the same arithmetic the scrapers would do.
//
// Verified against the collector image the chart pins (contrib 0.158.0), run
// with the chart's own scraper configuration against a ClickHouse holding this
// schema: the names, the `state` attribute and its values are what actually
// landed in otel_metrics_sum, not what the documentation says should.
const (
	MetricHostCPUTime         = "system.cpu.time"
	MetricHostMemoryUsage     = "system.memory.usage"
	MetricHostFilesystemUsage = "system.filesystem.usage"
)

// The attributes those metrics are broken down by. `state` splits every one of
// them; the filesystem metric adds the mount it describes.
const (
	hostStateAttribute      = "state"
	hostMountPointAttribute = "mountpoint"
	hostDeviceAttribute     = "device"
)

// The states this reader singles out of each breakdown.
//
// `idle` is the one CPU state that is not work, so the busy fraction is what is
// left of the whole. `used` is memory that is not reclaimable — the free,
// cached, buffered and slab states are the rest of the total, and counting the
// page cache as used would report every long-running node as full. On a
// filesystem, `used` and `free` are what a writer sees; the third state,
// `reserved`, is the root reservation nothing can write into, which is why the
// capacity below is `used + free` rather than the device's size. That is also
// how the scraper's own `system.filesystem.utilization` would compute it.
const (
	hostStateIdle = "idle"
	hostStateUsed = "used"
	hostStateFree = "free"
)

// NodeUsageQuery asks what the nodes were doing over a window.
//
// It names no node: there are a handful of them, the question is asked about
// all of them at once, and a per-node narrowing would only be a filter a caller
// can apply to the answer.
type NodeUsageQuery struct {
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	// Bucket is how wide one point is, rounded up to the next rung of the
	// resource ladder so that a node's series and an environment's can be read
	// against each other. Zero takes the rollup's width.
	Bucket time.Duration
}

// NodeUsagePoint is one bucket of a node series.
//
// Observed is the field that earns its place, and it is why this is not a bare
// float: a bucket the collector missed and a bucket in which the node was doing
// nothing must not read the same. A missing scrape rendered as a zero would end
// every sustained run at the first blink, and the rules that read these series
// are built on runs.
type NodeUsagePoint struct {
	Start time.Time `json:"start"`
	// Value is a fraction in 0..1.
	Value    float64 `json:"value"`
	Observed bool    `json:"observed"`
}

// NodeFilesystemUsage is one mounted filesystem's fill over the window.
//
// One entry per (mount point, device) the collector kept — the chart's scraper
// excludes the thousand image-layer and projected-volume mounts a node also
// carries, so these are its real disks.
type NodeFilesystemUsage struct {
	MountPoint string `json:"mountPoint"`
	Device     string `json:"device"`
	// CapacityBytes is what a writer can use: the used and free bytes the
	// newest bucket reported, which excludes the root reservation.
	CapacityBytes uint64 `json:"capacityBytes"`
	// Used is the used fraction per bucket, oldest first.
	Used []NodeUsagePoint `json:"used"`
}

// NodeUsage is one node's saturation over the window, bucketed.
type NodeUsage struct {
	Node string `json:"node"`
	// Start is the first bucket's start and End the window's end; both are
	// snapped the way every other bucketed read in this package snaps them.
	Start         time.Time `json:"start"`
	End           time.Time `json:"end"`
	BucketSeconds int       `json:"bucketSeconds"`
	// CPU and Memory are utilisation fractions in 0..1, oldest bucket first,
	// with every bucket of the window present.
	CPU         []NodeUsagePoint      `json:"cpu"`
	Memory      []NodeUsagePoint      `json:"memory"`
	Filesystems []NodeFilesystemUsage `json:"filesystems"`
}

// NodeUsage buckets every node's CPU, memory and filesystem fill across a
// window.
//
// It is two statements rather than one because the two answers are grouped
// differently — saturation is per node, fill is per mount — and a join that
// carried the mount points through the saturation grouping would multiply every
// CPU bucket by the number of filesystems on the node.
func (c *Client) NodeUsage(ctx context.Context, query NodeUsageQuery) ([]NodeUsage, error) {
	since, until, err := resolveWindow(query.Since, query.Until)
	if err != nil {
		return nil, err
	}
	width := nodeBucketSeconds(query.Bucket)

	// ClickHouse's toStartOfInterval counts from the Unix epoch, so the buckets
	// filled in here have to as well. See ResourceSeries for the same alignment
	// and why Go's Truncate is not it.
	start := time.Unix(since.Unix()-since.Unix()%int64(width), 0).UTC()
	step := time.Duration(width) * time.Second
	// A window that would need more points than the ceiling is answered as far
	// as the ceiling reaches, which is the bound ResourceSeries applies to the
	// same arithmetic. Asking for a window that wide at a bucket that narrow is
	// asking for a chart nobody can read.
	count := int(until.Sub(start)/step) + 1
	if count > MaxResourceBuckets {
		count = MaxResourceBuckets
	}

	params := map[string]string{
		"since": since.Format(time.RFC3339Nano),
		"until": until.Format(time.RFC3339Nano),
		"width": strconv.Itoa(width),
	}

	series := &nodeSeries{start: start, end: until, step: step, count: count, width: width}
	rows, err := c.selectionRows(ctx, nodeSaturationStatement(c.cfg.Database), params)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		index, ok := series.index(row["bucket"])
		if !ok {
			continue
		}
		usage := series.node(row["node"])
		// The busy fraction is what is not idle, over the whole of the CPU time
		// the bucket accounted for. A bucket holding one sample has no delta to
		// divide, which is an unobserved bucket rather than an idle node.
		if total := parseFloat(row["cpuTotal"]); total > 0 {
			usage.CPU[index] = observedFraction(usage.CPU[index].Start,
				(total-parseFloat(row["cpuIdle"]))/total)
		}
		if total := parseFloat(row["memoryTotal"]); total > 0 {
			usage.Memory[index] = observedFraction(usage.Memory[index].Start,
				parseFloat(row["memoryUsed"])/total)
		}
	}

	rows, err = c.selectionRows(ctx, nodeFilesystemStatement(c.cfg.Database), params)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		index, ok := series.index(row["bucket"])
		if !ok {
			continue
		}
		// The columns are named after the attributes they carry, but it is the
		// aliases in the statement that these keys have to match — the attribute
		// names reach the statement as map keys, not as aliases.
		filesystem := series.filesystem(row["node"], row["mountpoint"], row["device"])
		used, free := parseFloat(row["used"]), parseFloat(row["free"])
		capacity := used + free
		if capacity <= 0 {
			continue
		}
		filesystem.Used[index] = observedFraction(filesystem.Used[index].Start, used/capacity)
		// The rows arrive oldest first, so the last write is the newest bucket
		// that reported — a filesystem that was grown mid-window is read
		// against the size it has now.
		filesystem.CapacityBytes = uint64(capacity)
	}

	return series.collect(), nil
}

// nodeSaturationStatement is CPU and memory per node per bucket.
//
// The two levels are what keep the two metrics from contaminating each other. A
// metric table is one row per metric per state per scrape, so the inner grouping
// reduces each state to one number per bucket — a delta for the CPU counter,
// which is cumulative seconds, and a level for the memory gauge-in-a-sum, which
// is bytes — and the outer one adds the states up per metric. Summing them in
// one pass would add CPU seconds to bytes.
//
// `slot` is deliberately not called `bucket`: the outer SELECT renders the
// bucket as a string and a GROUP BY resolves against the SELECT aliases first,
// so grouping by that name would group by the rendered text. Every column is
// read through the table's alias for the same family of reasons — see
// createMetricsRollupGaugeView.
func nodeSaturationStatement(database string) string {
	return fmt.Sprintf(`SELECT
    node,
    toString(toUnixTimestamp(slot)) AS bucket,
    toString(sumIf(delta, metric = %[3]s)) AS cpuTotal,
    toString(sumIf(delta, metric = %[3]s AND state = %[5]s)) AS cpuIdle,
    toString(sumIf(level, metric = %[4]s)) AS memoryTotal,
    toString(sumIf(level, metric = %[4]s AND state = %[6]s)) AS memoryUsed
FROM (
    SELECT
        h.node AS node,
        toStartOfInterval(h.TimeUnix, toIntervalSecond({width:UInt32})) AS slot,
        h.MetricName AS metric,
        h.Attributes[%[7]s] AS state,
        max(h.Value) - min(h.Value) AS delta,
        avg(h.Value) AS level
    FROM %[1]s.%[2]s AS h
    WHERE h.MetricName IN (%[3]s, %[4]s)
      AND h.node != ''
      AND h.TimeUnix >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND h.TimeUnix <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY node, slot, metric, state
)
GROUP BY node, slot
ORDER BY node, slot
FORMAT JSONEachRow`,
		quoteIdentifier(database), quoteIdentifier(MetricsSumTable),
		quoteLiteral(MetricHostCPUTime), quoteLiteral(MetricHostMemoryUsage),
		quoteLiteral(hostStateIdle), quoteLiteral(hostStateUsed),
		quoteLiteral(hostStateAttribute))
}

// nodeFilesystemStatement is every mount point's used and free bytes per
// bucket.
//
// The mean within a bucket rather than the last sample: a filesystem's fill is
// noisy at scrape resolution, and the straight-line fit behind
// `node.disk-filling` is only as good as the points it is given.
func nodeFilesystemStatement(database string) string {
	return fmt.Sprintf(`SELECT
    node,
    toString(toUnixTimestamp(slot)) AS bucket,
    mountpoint,
    device,
    toString(used) AS used,
    toString(free) AS free
FROM (
    SELECT
        f.node AS node,
        toStartOfInterval(f.TimeUnix, toIntervalSecond({width:UInt32})) AS slot,
        f.Attributes[%[4]s] AS mountpoint,
        f.Attributes[%[5]s] AS device,
        avgIf(f.Value, f.Attributes[%[6]s] = %[7]s) AS used,
        avgIf(f.Value, f.Attributes[%[6]s] = %[8]s) AS free
    FROM %[1]s.%[2]s AS f
    WHERE f.MetricName = %[3]s
      AND f.node != ''
      AND f.TimeUnix >= parseDateTimeBestEffort({since:String}, 'UTC')
      AND f.TimeUnix <= parseDateTimeBestEffort({until:String}, 'UTC')
    GROUP BY node, slot, mountpoint, device
)
ORDER BY node, mountpoint, device, slot
FORMAT JSONEachRow`,
		quoteIdentifier(database), quoteIdentifier(MetricsSumTable),
		quoteLiteral(MetricHostFilesystemUsage),
		quoteLiteral(hostMountPointAttribute), quoteLiteral(hostDeviceAttribute),
		quoteLiteral(hostStateAttribute), quoteLiteral(hostStateUsed), quoteLiteral(hostStateFree))
}

// nodeSeries assembles the two answers into one series per node.
//
// Every series is created full — one point per bucket of the window, none of
// them observed — and the rows fill in the buckets that reported. That is what
// makes a gap in the answer a gap in the series rather than a shorter series.
type nodeSeries struct {
	start time.Time
	end   time.Time
	step  time.Duration
	count int
	width int
	nodes map[string]*NodeUsage
	// mounts indexes a node's filesystems by mount point and device, so a
	// second bucket for the same mount lands on the entry the first one made,
	// and order keeps them in the order the store named them.
	//
	// They are pointers held apart from the NodeUsage they belong to, and
	// deliberately: appending a second filesystem to a slice of values can move
	// the backing array, and every pointer taken into the old one would then be
	// written to and thrown away. The values are copied in at [collect].
	mounts map[string]*NodeFilesystemUsage
	order  map[string][]*NodeFilesystemUsage
}

// index places a bucket key from the store into the series, or reports that it
// falls outside the window the caller asked for.
func (s *nodeSeries) index(bucket string) (int, bool) {
	unix, err := strconv.ParseInt(bucket, 10, 64)
	if err != nil {
		return 0, false
	}
	index := int(time.Unix(unix, 0).UTC().Sub(s.start) / s.step)
	if index < 0 || index >= s.count {
		return 0, false
	}
	return index, true
}

func (s *nodeSeries) node(name string) *NodeUsage {
	if s.nodes == nil {
		s.nodes = map[string]*NodeUsage{}
	}
	usage := s.nodes[name]
	if usage == nil {
		usage = &NodeUsage{
			Node:          name,
			Start:         s.start,
			End:           s.end,
			BucketSeconds: s.width,
			CPU:           s.points(),
			Memory:        s.points(),
		}
		s.nodes[name] = usage
	}
	return usage
}

func (s *nodeSeries) filesystem(node, mountPoint, device string) *NodeFilesystemUsage {
	s.node(node)
	if s.mounts == nil {
		s.mounts = map[string]*NodeFilesystemUsage{}
		s.order = map[string][]*NodeFilesystemUsage{}
	}
	// The three parts of the key are joined by a byte none of them can contain,
	// so two mounts cannot be folded into one by their names running together.
	key := node + "\x00" + mountPoint + "\x00" + device
	filesystem := s.mounts[key]
	if filesystem == nil {
		filesystem = &NodeFilesystemUsage{MountPoint: mountPoint, Device: device, Used: s.points()}
		s.mounts[key] = filesystem
		s.order[node] = append(s.order[node], filesystem)
	}
	return filesystem
}

// points is an empty series: every bucket of the window, none observed.
func (s *nodeSeries) points() []NodeUsagePoint {
	points := make([]NodeUsagePoint, s.count)
	for i := range points {
		points[i].Start = s.start.Add(time.Duration(i) * s.step)
	}
	return points
}

// collect returns the nodes in name order, so that two evaluations of an
// unchanged cluster produce the same answer.
func (s *nodeSeries) collect() []NodeUsage {
	names := make([]string, 0, len(s.nodes))
	for name := range s.nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	usage := make([]NodeUsage, 0, len(names))
	for _, name := range names {
		node := *s.nodes[name]
		node.Filesystems = make([]NodeFilesystemUsage, 0, len(s.order[name]))
		for _, filesystem := range s.order[name] {
			node.Filesystems = append(node.Filesystems, *filesystem)
		}
		usage = append(usage, node)
	}
	return usage
}

// observedFraction is a bucket that reported, with its value held inside 0..1.
//
// The clamp is not decoration, and it is not load-bearing either. Both
// fractions are bounded by construction as long as the samples are — the idle
// delta is part of the total it is taken out of, and used memory is one of the
// states the total sums — so this only ever fires on a source that has already
// contradicted itself. It is here so that when one does, a node renders at
// 100% rather than at 4000%.
func observedFraction(start time.Time, value float64) NodeUsagePoint {
	switch {
	case value < 0:
		value = 0
	case value > 1:
		value = 1
	}
	return NodeUsagePoint{Start: start, Value: value, Observed: true}
}

// nodeBucketSeconds rounds a requested bucket width up to the next rung of the
// resource ladder. A node series and an environment's are read side by side, so
// they are quantised the same way rather than each to their own idea of a
// resolution.
func nodeBucketSeconds(width time.Duration) int {
	if width <= 0 {
		return MetricsRollupSeconds
	}
	wanted := int(width.Seconds())
	for _, rung := range resourceBucketLadder {
		if rung >= wanted {
			return rung
		}
	}
	return resourceBucketLadder[len(resourceBucketLadder)-1]
}

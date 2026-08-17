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
	"strings"
	"testing"
	"time"
)

// The claim every volume test is about, and the namespace it is mounted in.
const (
	testClaim     = "shop-data"
	testNamespace = "kitchen-app-shop"
)

// Used bytes are not a metric. The receiver has none — asking for
// `k8s.volume.used` is refused at startup as an unknown metric — so the fill is
// derived from the two gauges that do exist, which is the same arithmetic the
// kubelet summary behind them does.
func TestVolumeUsageDerivesTheFillFromCapacityAndAvailable(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = `{"namespace":"` + testNamespace + `","claim":"` + testClaim + `","project":"shop",` +
		`"environment":"production","pod":"shop-0","capacity":"10737418240","available":"1073741824"}`

	volumes, err := store.client(t).VolumeUsage(context.Background(), VolumeUsageQuery{
		At: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("VolumeUsage: %v", err)
	}
	if len(volumes) != 1 {
		t.Fatalf("want one claim, got %d", len(volumes))
	}
	volume := volumes[0]
	if volume.Claim != testClaim || volume.Namespace != testNamespace || volume.Pod != "shop-0" {
		t.Errorf("a volume names the claim, not the mount: %+v", volume)
	}
	if volume.UsedBytes != 9663676416 || volume.CapacityBytes != 10737418240 {
		t.Errorf("used should be capacity less available: %+v", volume)
	}
	if volume.UsedFraction != 0.9 {
		t.Errorf("want 0.9 of the claim used, got %v", volume.UsedFraction)
	}
	// The project comes off the pod the collector enriched, which is what lets
	// a finding be attributed to an application rather than to a namespace.
	if volume.Project != testProject || volume.Environment != testEnvironment {
		t.Errorf("the claim should carry what the pod was labelled with: %+v", volume)
	}
}

// Every pod mounts a projected service-account token, and most mount a
// configMap or two. None of them is a claim, and the attribute that names a
// claim is the only thing that tells them apart.
func TestVolumeUsageOnlyCountsWhatIsBoundToAClaim(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).VolumeUsage(context.Background(), VolumeUsageQuery{}); err != nil {
		t.Fatalf("VolumeUsage: %v", err)
	}
	if !strings.Contains(store.query, "v.ResourceAttributes["+quoteLiteral(VolumeClaimAttribute)+"] != ''") {
		t.Errorf("a volume with no claim behind it is not a claim:\n%s", store.query)
	}
	for _, metric := range []string{MetricVolumeCapacity, MetricVolumeAvailable} {
		if !strings.Contains(store.query, quoteLiteral(metric)) {
			t.Errorf("the read never asks for %s:\n%s", metric, store.query)
		}
	}
	// The volume metrics are gauges, and the claim's name is a resource
	// attribute rather than a materialized column — there is no column for it.
	if !strings.Contains(store.query, qualified(MetricsGaugeTable)) {
		t.Errorf("the volume metrics are gauges:\n%s", store.query)
	}
	// The two numbers are separate rows of the same scrape, so each is taken at
	// the newest sample that carried it. A max() of each would pair the
	// capacity of one scrape with the free space of another.
	if !strings.Contains(store.query, "argMaxIf(v.Value, v.TimeUnix,") {
		t.Errorf("both numbers should come off the newest sample:\n%s", store.query)
	}
	// One row per claim, not per mount: a claim mounted by three pods is one
	// volume with one fill, and three findings would be the same warning three
	// times.
	if !strings.Contains(store.query, "GROUP BY namespace, claim") {
		t.Errorf("the answer is per claim:\n%s", store.query)
	}
}

// The window is bounded so the scan is, and the bound is also what makes the
// answer current: a claim whose newest sample is older than the lookback is on
// a node that has gone quiet, which is node.silent's subject and not a fill
// figure to keep repeating.
func TestVolumeUsageBoundsTheLookbackAndTheRows(t *testing.T) {
	store := newFakeLogStore(t)
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

	if _, err := store.client(t).VolumeUsage(context.Background(), VolumeUsageQuery{At: at}); err != nil {
		t.Fatalf("VolumeUsage: %v", err)
	}
	since, err := time.Parse(time.RFC3339Nano, store.params.Get("param_since"))
	if err != nil {
		t.Fatalf("the lookback should travel as a parameter: %v", err)
	}
	if !since.Equal(at.Add(-DefaultVolumeLookback)) {
		t.Errorf("want a %s lookback ending at %s, got %s", DefaultVolumeLookback, at, since)
	}
	if got := store.params.Get("param_limit"); got != "500" {
		t.Errorf("the row cap should travel as a parameter, got %q", got)
	}
	// The cap can only ever drop claims with room to spare.
	if !strings.Contains(store.query, "DESC") || !strings.Contains(store.query, "LIMIT {limit:UInt32}") {
		t.Errorf("the fullest claims should survive the cap:\n%s", store.query)
	}
}

// A volume whose capacity has not been reported is not a volume at 100%; it is
// a volume nothing has measured, and reporting it would be inventing the one
// number the signal is about.
func TestVolumeUsageSkipsAClaimWithNoCapacity(t *testing.T) {
	store := newFakeLogStore(t)
	store.rows = strings.Join([]string{
		`{"namespace":"` + testNamespace + `","claim":"unmeasured","project":"","environment":"",` +
			`"pod":"shop-0","capacity":"0","available":"0"}`,
		`{"namespace":"` + testNamespace + `","claim":"` + testClaim + `","project":"","environment":"",` +
			`"pod":"shop-0","capacity":"100","available":"120"}`,
	}, "\n")

	volumes, err := store.client(t).VolumeUsage(context.Background(), VolumeUsageQuery{})
	if err != nil {
		t.Fatalf("VolumeUsage: %v", err)
	}
	if len(volumes) != 1 {
		t.Fatalf("want the measured claim alone, got %+v", volumes)
	}
	// More available than capacity is a source contradicting itself; an empty
	// volume is the honest reading of it, and a negative one is not a number.
	if volumes[0].UsedBytes != 0 || volumes[0].UsedFraction != 0 {
		t.Errorf("want an empty volume, got %+v", volumes[0])
	}
}

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
	"fmt"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// projectTraffic builds the per-project answer the cross-project detectors
// compare between two windows.
func projectTraffic(count int, p95, errorRate float64) []clickhouse.ProjectTraffic {
	traffic := make([]clickhouse.ProjectTraffic, 0, count)
	for i := 0; i < count; i++ {
		traffic = append(traffic, clickhouse.ProjectTraffic{
			Project:   fmt.Sprintf("project-%d", i),
			Requests:  1000,
			P95Ms:     p95,
			ErrorRate: errorRate,
		})
	}
	return traffic
}

func TestLatencyCorrelatedFiresAcrossProjects(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ProjectTrafficBaseline = projectTraffic(4, 300, 0)
	snapshot.ProjectTrafficRecent = projectTraffic(4, 1200, 0)

	finding := expectOne(t, evaluate(t, SignalLatencyCorrelated, snapshot))
	if finding.Title != "p95 degraded across 4 projects" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "check node saturation and the edge")
}

// Two projects sharing a node is a coincidence worth nothing.
func TestLatencyCorrelatedNeedsThreeProjects(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ProjectTrafficBaseline = projectTraffic(2, 300, 0)
	snapshot.ProjectTrafficRecent = projectTraffic(2, 1200, 0)
	expectNone(t, evaluate(t, SignalLatencyCorrelated, snapshot))
}

// Which projects are caught up in a platform problem changes minute to minute
// as traffic moves; the problem does not, so the fingerprint must not list
// them.
func TestLatencyCorrelatedFingerprintDoesNotMoveWithTheProjectSet(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ProjectTrafficBaseline = projectTraffic(3, 300, 0)
	snapshot.ProjectTrafficRecent = projectTraffic(3, 1200, 0)
	first := expectOne(t, evaluate(t, SignalLatencyCorrelated, snapshot))

	snapshot.ProjectTrafficBaseline = projectTraffic(6, 300, 0)
	snapshot.ProjectTrafficRecent = projectTraffic(6, 1200, 0)
	second := expectOne(t, evaluate(t, SignalLatencyCorrelated, snapshot))

	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint moved with the project set: %q then %q",
			first.Fingerprint, second.Fingerprint)
	}
}

func TestErrorCorrelatedFiresAcrossProjects(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ProjectTrafficBaseline = projectTraffic(3, 100, 0.001)
	snapshot.ProjectTrafficRecent = projectTraffic(3, 100, 0.25)

	expectDetail(t, expectOne(t, evaluate(t, SignalErrorCorrelated, snapshot)),
		"something they share")
}

// A project that served nine requests cannot corroborate anything.
func TestErrorCorrelatedIgnoresQuietProjects(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ProjectTrafficBaseline = projectTraffic(4, 100, 0.001)
	snapshot.ProjectTrafficRecent = projectTraffic(4, 100, 0.25)
	for i := range snapshot.ProjectTrafficRecent {
		snapshot.ProjectTrafficRecent[i].Requests = 3
	}
	expectNone(t, evaluate(t, SignalErrorCorrelated, snapshot))
}

// The rule re-derives nothing: it reads the survey the Kitchen reconciler
// already wrote, so the two screens cannot disagree about whether the platform
// is up.
func TestComponentUnhealthyReadsTheSurvey(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.Components = []kitchenv1alpha1.ComponentStatus{
		{Name: "auth", Kind: "Deployment", Healthy: true, Desired: 1, Available: 1},
		{
			Name: "logs", Kind: "DaemonSet", Healthy: false, Desired: 3, Available: 0,
			Message: `0 of 3 pods available: violates PodSecurity "baseline:latest"`,
		},
	}

	finding := expectOne(t, evaluate(t, SignalComponentUnhealthy, snapshot))
	if finding.Fingerprint != "platform.component-unhealthy/logs" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	expectDetail(t, finding, "violates PodSecurity")
}

func TestComponentUnhealthyStaysQuietWhenEverythingIsUp(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.Components = []kitchenv1alpha1.ComponentStatus{
		{Name: "auth", Kind: "Deployment", Healthy: true, Desired: 1, Available: 1},
	}
	expectNone(t, evaluate(t, SignalComponentUnhealthy, snapshot))
}

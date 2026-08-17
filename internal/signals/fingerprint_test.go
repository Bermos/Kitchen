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
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// This is the test the rest of the package is arranged around.
//
// A fingerprint that moves for a condition that has not changed is worse than
// no fingerprint at all: the background loop stage 5 adds would record the
// problem resolving and reopening on every interval, and the inbox built on
// those transitions would be noise from the first hour. So the whole catalogue
// is evaluated twice over a cluster that is broken in a dozen ways, with the
// clock advanced and every series regenerated around the new instant, and the
// two rounds must name the same conditions.

// nearMemoryLimit is 97% of the test limit, which is past
// MemoryLimitFraction.
const nearMemoryLimit = testMemoryLimit * 97 / 100

// brokenSnapshot is a platform with something wrong in every group of the
// catalogue, built relative to `now` so that the same cluster can be described
// at two instants.
func brokenSnapshot() *Snapshot { return brokenSnapshotAt(testNow) }

func brokenSnapshotAt(now time.Time) *Snapshot {
	// Every series builder in helpers_test.go is anchored on testNow, so the
	// offset is applied to the snapshot's own instant and to the objects, and
	// the series are shifted by the same amount below.
	shift := now.Sub(testNow)

	snapshot := &Snapshot{
		Now:       now,
		Traffic:   map[EnvKey]clickhouse.RequestSeries{},
		Resources: map[EnvKey]clickhouse.ResourceSeries{},
		Freshness: map[string]time.Time{},
		NodeUsage: map[string]NodeUsage{},
		Environments: []kitchenv1alpha1.Environment{{
			ObjectMeta: metav1.ObjectMeta{Name: testEnvironment},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
			},
			Status: kitchenv1alpha1.EnvironmentStatus{URL: "https://" + testHost},
		}},
	}
	snapshot.Platform = PlatformFacts{
		BaseDomain:     "example.com",
		GatewayAddress: testGatewayIP,
		RetentionDays:  30,
		Components: []kitchenv1alpha1.ComponentStatus{{
			Name: "logs", Kind: "DaemonSet", Desired: 3, Available: 0,
			Message: `violates PodSecurity "baseline:latest"`,
		}},
	}

	// A crash-looping container, an unschedulable pod, and a workload with no
	// pods at all whose rejection is only in an event.
	looping := waitingPod(reasonCrashLoopBackOff, "back-off 5m0s")
	looping.Status.ContainerStatuses[0].RestartCount = 12
	pending := appPod("shop-pr-41-def")
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionFalse,
		Reason:             "Unschedulable",
		Message:            "0/2 nodes are available: 2 Insufficient memory",
		LastTransitionTime: metav1.NewTime(now.Add(-time.Hour)),
	}}
	snapshot.Pods = []corev1.Pod{looping, *pending}

	snapshot.Deployments = []appsv1.Deployment{cloudflared(0)}
	snapshot.DaemonSets = []appsv1.DaemonSet{{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kitchen-collector", Namespace: controller.PlatformNamespace,
			CreationTimestamp: metav1.NewTime(now.Add(-6 * time.Hour)),
		},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3},
	}}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{{
		Timestamp: now.Add(-40 * time.Minute),
		Namespace: controller.PlatformNamespace,
		Kind:      "DaemonSet",
		Name:      "kitchen-collector",
		Reason:    reasonFailedCreate,
		Message:   `violates PodSecurity "baseline:latest": hostPath volumes`,
		Count:     18,
	}, {
		Timestamp: now.Add(-20 * time.Minute),
		Namespace: controller.PlatformNamespace,
		Kind:      "Pod",
		Name:      "kitchen-clickhouse-0",
		Reason:    "FailedMount",
		Message:   testMountMessage,
		Count:     4,
	}}

	// A node that is silent and saturated, and a claim that never bound.
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi"), node(testOtherNode, "4", "16Gi")}
	snapshot.Freshness[testOtherNode] = now.Add(-time.Minute)
	snapshot.NodeUsage[testOtherNode] = shiftUsage(usageOf(testOtherNode, flat(0.97), flat(0.5)), shift)
	snapshot.Claims = []corev1.PersistentVolumeClaim{
		claim(controller.PlatformNamespace, "data-kitchen-clickhouse-0", corev1.ClaimPending),
	}
	snapshot.Store = StoreHealth{
		BytesOnDisk: 95 << 30, CapacityBytes: 100 << 30, NewestRow: now.Add(-time.Minute),
	}

	// The edge: an unprogrammed Gateway, a certificate that will not renew, a
	// stale name still pointing here.
	snapshot.Gateways = []gatewayv1.Gateway{unprogrammedGateway(reasonAddressNotAssigned, "no address")}
	snapshot.Certificates = []Certificate{{
		Namespace: controller.PlatformNamespace,
		Name:      "kitchen-wildcard",
		DNSNames:  []string{"*.example.com"},
		NotAfter:  now.Add(4 * 24 * time.Hour),
		Reason:    "Failed",
		Message:   "propagation check failed",
	}}
	snapshot.DNS = []DNSProbe{{Host: testHost, Addresses: []string{"198.51.100.7"}, Exists: true}}
	snapshot.UnroutedHosts = []clickhouse.UnroutedHost{{
		Host: "old.example.com", Requests: 9000,
		FirstSeen: now.Add(-55 * time.Minute), LastSeen: now.Add(-time.Minute),
	}}

	// Builds failing in a row.
	for i := 0; i < 4; i++ {
		failed := build("build-"+string(rune('a'+i)), kitchenv1alpha1.BuildFailed,
			time.Duration(i+1)*time.Hour)
		failed.CreationTimestamp = metav1.NewTime(now.Add(-time.Duration(i+1) * time.Hour))
		snapshot.Builds = append(snapshot.Builds, failed)
	}

	// The environment's own traffic and resources.
	snapshot.Traffic[testKey] = shiftTraffic(trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{Requests: 200, Errors: 80, ErrorRate: 0.40, P95Ms: 1600}
		}
		return clickhouse.RequestPoint{Requests: 200, Errors: 2, ErrorRate: 0.01, P95Ms: 200}
	}), shift)
	snapshot.Resources[testKey] = shiftResources(resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			point := clickhouse.ResourcePoint{
				MemoryPeakBytes: nearMemoryLimit,
				CPUPeakCores:    0.99,
			}
			if i == resourceBuckets-3 {
				point.OOMKills = 1
			}
			return point
		}), shift)

	// Three projects degrading together.
	snapshot.ProjectTrafficBaseline = projectTraffic(3, 200, 0.001)
	snapshot.ProjectTrafficRecent = projectTraffic(3, 1600, 0.30)
	return snapshot
}

// shiftTraffic, shiftResources and shiftUsage move a series built around
// testNow to a different instant, so that the same cluster can be described at
// two times without any of the builders learning about a clock.
func shiftTraffic(series clickhouse.RequestSeries, by time.Duration) clickhouse.RequestSeries {
	series.Start = series.Start.Add(by)
	series.End = series.End.Add(by)
	for i := range series.Points {
		series.Points[i].Start = series.Points[i].Start.Add(by)
	}
	return series
}

func shiftResources(series clickhouse.ResourceSeries, by time.Duration) clickhouse.ResourceSeries {
	series.Start = series.Start.Add(by)
	series.End = series.End.Add(by)
	for i := range series.Points {
		series.Points[i].Start = series.Points[i].Start.Add(by)
	}
	return series
}

func shiftUsage(usage NodeUsage, by time.Duration) NodeUsage {
	for i := range usage.CPU {
		usage.CPU[i].Start = usage.CPU[i].Start.Add(by)
	}
	for i := range usage.Memory {
		usage.Memory[i].Start = usage.Memory[i].Start.Add(by)
	}
	return usage
}

func TestFingerprintsSurviveAnEvaluationRound(t *testing.T) {
	first := fingerprintsOf(Catalogue().Evaluate(brokenSnapshotAt(testNow)))
	// A round later: the clock has moved, every bucket boundary with it, and
	// nothing about the cluster has changed.
	second := fingerprintsOf(Catalogue().Evaluate(brokenSnapshotAt(testNow.Add(7 * time.Minute))))

	if len(first) == 0 {
		t.Fatal("the broken snapshot produced no findings, so this test proves nothing")
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("fingerprints moved between rounds:\nfirst:\n%s\nsecond:\n%s",
			strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
}

// The broken snapshot is meant to exercise most of the catalogue, so it is
// worth asserting that it does: a stability test over three findings would
// pass while the other thirty-three churned.
func TestTheBrokenSnapshotExercisesMostOfTheCatalogue(t *testing.T) {
	fired := map[ID]bool{}
	for _, finding := range Catalogue().Evaluate(brokenSnapshot()) {
		if finding.Severity != SeverityUnknown {
			fired[finding.Signal] = true
		}
	}
	const wanted = 20
	if len(fired) < wanted {
		missing := make([]string, 0, len(catalogueV1))
		for _, id := range catalogueV1 {
			if !fired[id] {
				missing = append(missing, string(id))
			}
		}
		sort.Strings(missing)
		t.Fatalf("only %d signals fired, want at least %d; quiet: %s",
			len(fired), wanted, strings.Join(missing, ", "))
	}
}

// Every finding must carry somewhere to look. A problem without its evidence
// is a sentence about a number nobody can check.
func TestEveryFindingCarriesEvidenceAndAFingerprintNamingItsSignal(t *testing.T) {
	for _, finding := range Catalogue().Evaluate(brokenSnapshot()) {
		if finding.Evidence == "" {
			t.Errorf("%s carries no evidence link", finding.Fingerprint)
		}
		if finding.Title == "" {
			t.Errorf("%s carries no title", finding.Fingerprint)
		}
		if !strings.HasPrefix(finding.Fingerprint, string(finding.Signal)) {
			t.Errorf("fingerprint %q does not begin with its signal %q",
				finding.Fingerprint, finding.Signal)
		}
		if finding.Since.IsZero() {
			t.Errorf("%s is undated", finding.Fingerprint)
		}
	}
}

// No two findings in one round may share a fingerprint, or the diff would
// collapse two conditions into one.
func TestFingerprintsAreUniqueWithinARound(t *testing.T) {
	seen := map[string]Finding{}
	for _, finding := range Catalogue().Evaluate(brokenSnapshot()) {
		if previous, taken := seen[finding.Fingerprint]; taken {
			t.Fatalf("two findings share %q: %q and %q",
				finding.Fingerprint, previous.Title, finding.Title)
		}
		seen[finding.Fingerprint] = finding
	}
}

func fingerprintsOf(findings Findings) []string {
	fingerprints := make([]string, 0, len(findings))
	for _, finding := range findings {
		fingerprints = append(fingerprints, fingerprint(finding))
	}
	sort.Strings(fingerprints)
	return fingerprints
}

// fingerprint renders the finding's identity plus its severity, since a
// condition that changed severity has changed even though it is the same
// condition — and a stability test that ignored that would hide it.
func fingerprint(finding Finding) string {
	return string(finding.Severity) + " " + finding.Fingerprint
}

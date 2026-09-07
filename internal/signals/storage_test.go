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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The message the CSI path raises when a volume will not come up, which is the
// only place the reason exists.
const testMountMessage = `Unable to attach or mount volumes: unmounted volumes=[data], ` +
	`timed out waiting for the condition`

// testStoreClaim is the claim the bundled telemetry store writes to, named the
// way the chart names it.
const testStoreClaim = "data-kitchen-clickhouse-0"

func claim(namespace, name string, phase corev1.PersistentVolumeClaimPhase) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(testNow.Add(-40 * time.Minute)),
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

// The default-StorageClass suspect is the whole reason this rule exists: a
// first install on a cluster without one binds nothing, and nothing anywhere
// says the words.
func TestPVCPendingNamesTheDefaultStorageClass(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Claims = []corev1.PersistentVolumeClaim{
		claim(controller.PlatformNamespace, testStoreClaim, corev1.ClaimPending),
	}

	finding := expectOne(t, evaluate(t, SignalPVCPending, snapshot))
	expectDetail(t, finding, "no default StorageClass")
	expectDetail(t, finding, "needs the cluster's default one")
}

// A claim a project asked for is attributed to the project, and names the
// class it wanted rather than the default.
func TestPVCPendingNamesTheClassItAskedFor(t *testing.T) {
	snapshot := newSnapshot()
	pending := claim(controller.AppNamespace(testProject), testClaim, corev1.ClaimPending)
	class := "fast-ssd"
	pending.Spec.StorageClassName = &class
	snapshot.Claims = []corev1.PersistentVolumeClaim{pending}

	finding := expectOne(t, evaluate(t, SignalPVCPending, snapshot))
	expectDetail(t, finding, `asks for StorageClass "fast-ssd"`)
	if finding.Fingerprint != "pvc.pending/shop/data" {
		t.Fatalf("fingerprint = %q, want the project's", finding.Fingerprint)
	}
}

func TestPVCPendingStaysQuietOnABoundClaim(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Claims = []corev1.PersistentVolumeClaim{
		claim(controller.PlatformNamespace, "data", corev1.ClaimBound),
	}
	expectNone(t, evaluate(t, SignalPVCPending, snapshot))
}

func TestPVCFillingFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.VolumeUsage = []VolumeUsage{{
		Namespace:     controller.AppNamespace(testProject),
		Claim:         testClaim,
		Project:       testProject,
		CapacityBytes: 10 << 30,
		UsedBytes:     9 << 30,
		UsedFraction:  0.90,
	}}

	finding := expectOne(t, evaluate(t, SignalPVCFilling, snapshot))
	if finding.Fingerprint != "pvc.filling/shop/data" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	expectDetail(t, finding, "kubelet's volume stats")
}

func TestPVCFillingStaysQuietBelowTheThreshold(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.VolumeUsage = []VolumeUsage{{
		Namespace: controller.PlatformNamespace, Claim: testClaim, UsedFraction: 0.40,
	}}
	expectNone(t, evaluate(t, SignalPVCFilling, snapshot))
}

// One volume, one row: a mount that will not come up raises the same warning
// every two minutes for as long as the pod is retried.
func TestAttachFailedCollapsesRepeatedEvents(t *testing.T) {
	snapshot := newSnapshot()
	for i := 0; i < 5; i++ {
		snapshot.ClusterEvents = append(snapshot.ClusterEvents, clickhouse.K8sEvent{
			Timestamp: testNow.Add(-time.Duration(i) * 5 * time.Minute),
			Namespace: controller.PlatformNamespace,
			Kind:      "Pod",
			Name:      "kitchen-clickhouse-0",
			Reason:    "FailedMount",
			Message:   testMountMessage,
			Count:     3,
		})
	}

	finding := expectOne(t, evaluate(t, SignalAttachFailed, snapshot))
	expectDetail(t, finding, "15 failures")
	expectDetail(t, finding, "the CSI driver attaches it")
}

func TestAttachFailedIgnoresOtherWarnings(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.ClusterEvents = []clickhouse.K8sEvent{{
		Timestamp: testNow.Add(-time.Minute),
		Namespace: controller.PlatformNamespace,
		Reason:    "BackOff",
		Message:   "Back-off restarting failed container",
	}}
	expectNone(t, evaluate(t, SignalAttachFailed, snapshot))
}

// storeVolume is the store's claim as the kubelet measured it.
func storeVolume(capacity, used uint64) *VolumeUsage {
	return &VolumeUsage{
		Namespace:     controller.PlatformNamespace,
		Claim:         testStoreClaim,
		CapacityBytes: capacity,
		UsedBytes:     used,
		UsedFraction:  float64(used) / float64(capacity),
	}
}

// The bug this rule was written for and did not catch: the volume 89% full
// while the telemetry on it is a fraction of that, because something else —
// ClickHouse's own system tables — is the rest. The old ratio read 8.4% here
// and the rule stayed silent for ten hours (#531).
func TestStoreDiskFiresOnAFullVolumeWithASmallDatabase(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.RetentionDays = 30
	snapshot.Store = StoreHealth{
		BytesOnDisk:   1680 << 20, // 1.64 GiB of kitchen.* …
		CapacityBytes: 20 << 30,
		Claim:         testStoreClaim,
		Volume:        storeVolume(20<<30, (20<<30)/100*89), // … on a disk 89% full
	}

	finding := expectOne(t, evaluate(t, SignalStoreDisk, snapshot))
	if finding.Severity != SeverityCritical {
		t.Fatalf("severity = %q, want critical", finding.Severity)
	}
	if !strings.Contains(finding.Title, "89%") {
		t.Fatalf("title = %q, want the volume's fill", finding.Title)
	}
	expectDetail(t, finding, "on the store's volume "+testStoreClaim)
	// Both numbers, because a disk full of telemetry is a retention decision
	// and a disk full of something else is not.
	expectDetail(t, finding, "the telemetry itself is 1.6Gi of that")
	expectDetail(t, finding, "retention is 30 days")
	expectDetail(t, finding, "stops accepting writes")
}

// The inverse, which is the old rule's false positive: a database that fills
// most of the claim's nominal capacity on a volume that is nearly empty —
// a resized disk the claim has not caught up with. The kubelet measured the
// disk, and the disk is what this rule is about.
func TestStoreDiskStaysQuietOnAnEmptyVolume(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Store = StoreHealth{
		BytesOnDisk:   95 << 30,
		CapacityBytes: 100 << 30,
		Claim:         testStoreClaim,
		Volume:        storeVolume(500<<30, 100<<30),
	}
	expectNone(t, evaluate(t, SignalStoreDisk, snapshot))
}

// An external store's disk is not the platform's to judge, and a percentage of
// an unknown capacity is not a number. The gatherer marks the input
// not-applicable; a snapshot that never saw one produces nothing either way.
func TestStoreDiskStaysQuietWithoutAVolume(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Store = StoreHealth{BytesOnDisk: 900 << 30}
	expectNone(t, evaluate(t, SignalStoreDisk, snapshot))
}

// Nothing measured the disk: the rule reports that it could not be evaluated
// rather than dividing the database's own size by the claim's capacity, which
// is the fraction of two different things this rule used to answer with.
func TestStoreDiskReportsAnUnreadableVolume(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Store = StoreHealth{BytesOnDisk: 1 << 30, CapacityBytes: 20 << 30, Claim: testStoreClaim}
	snapshot.MarkUnreadable(InputStoreVolume,
		"the kubelet has reported no volume stats for claim "+testStoreClaim)

	finding := expectOne(t, evaluate(t, SignalStoreDisk, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}
	expectDetail(t, finding, "reports nothing rather than health")
}

func TestIngestStalledFiresWhilePodsRun(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{readyPod()}
	snapshot.Freshness[testNode] = testNow.Add(-45 * time.Minute)
	snapshot.Store.NewestRow = testNow.Add(-45 * time.Minute)

	finding := expectOne(t, evaluate(t, SignalIngestStalled, snapshot))
	expectDetail(t, finding, "newest row is 45m old")
}

// A platform with nothing scheduled genuinely has nothing to say, and
// reporting its silence would be reporting that it is switched off.
func TestIngestStalledStaysQuietOnAnIdleCluster(t *testing.T) {
	snapshot := newSnapshot()
	expectNone(t, evaluate(t, SignalIngestStalled, snapshot))
}

func TestIngestStalledStaysQuietWhileRowsArrive(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{readyPod()}
	snapshot.Freshness[testNode] = testNow.Add(-time.Minute)
	expectNone(t, evaluate(t, SignalIngestStalled, snapshot))
}

func TestFlowsLostContradictsTheRequestNumbers(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Ingest = IngestHealth{
		Window:    time.Hour,
		FlowsLost: 4200,
		LastLoss:  testNow.Add(-3 * time.Minute),
	}

	finding := expectOne(t, evaluate(t, SignalFlowsLost, snapshot))
	if finding.Title != "request counts are under-reporting" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "hubble.eventBufferCapacity")
}

func TestFlowsLostStaysQuietBelowTheThreshold(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Ingest = IngestHealth{Window: time.Hour, FlowsLost: 3}
	expectNone(t, evaluate(t, SignalFlowsLost, snapshot))
}

// A claim in a project's namespace belongs to the project, not to a namespace
// the reader has to decode.
func TestClaimScopeAttributesToTheProject(t *testing.T) {
	snapshot := newSnapshot()
	scope := claimScope(controller.AppNamespace(testProject), testClaim, snapshot)
	if scope.Project != testProject || scope.Namespace != "" {
		t.Fatalf("scope = %+v, want the project alone", scope)
	}
	platform := claimScope(controller.PlatformNamespace, testClaim, snapshot)
	if platform.Namespace != controller.PlatformNamespace || platform.Project != "" {
		t.Fatalf("scope = %+v, want the platform namespace", platform)
	}
}

// quantityValue is what turns a claim's reported capacity into the number
// store.disk divides by, and an absent capacity must be zero rather than a
// panic.
func TestQuantityValueToleratesAnAbsentCapacity(t *testing.T) {
	if got := quantityValue(nil); got != 0 {
		t.Fatalf("quantityValue(nil) = %d, want 0", got)
	}
	size := resource.MustParse("50Gi")
	if got := quantityValue(&size); got != 50<<30 {
		t.Fatalf("quantityValue(50Gi) = %d", got)
	}
}

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
		claim(controller.PlatformNamespace, "data-kitchen-clickhouse-0", corev1.ClaimPending),
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

func TestStoreDiskFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.RetentionDays = 30
	snapshot.Store = StoreHealth{BytesOnDisk: 90 << 30, CapacityBytes: 100 << 30}

	finding := expectOne(t, evaluate(t, SignalStoreDisk, snapshot))
	expectDetail(t, finding, "retention is 30 days")
	expectDetail(t, finding, "stops accepting writes")
}

// An external store's disk is not the platform's to judge, and a percentage of
// an unknown capacity is not a number.
func TestStoreDiskStaysQuietWithoutACapacity(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Store = StoreHealth{BytesOnDisk: 900 << 30}
	expectNone(t, evaluate(t, SignalStoreDisk, snapshot))
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

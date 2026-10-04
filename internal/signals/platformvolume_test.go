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
	"context"
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// testRegistryClaim is the bundled registry's claim, named the way the chart's
// StatefulSet names it.
const (
	testRegistryClaim = "data-kitchen-registry-0"
	testRegistrySet   = "kitchen-registry"
)

// registryVolume is the registry as status.storage.volumes records it.
func registryVolume() kitchenv1alpha1.PlatformVolumeStatus {
	return kitchenv1alpha1.PlatformVolumeStatus{
		StatefulSet: testRegistrySet,
		Component:   "registry",
		Claims:      []string{testRegistryClaim},
	}
}

// platformVolumeSnapshot is a platform whose registry volume the kubelet
// measured at the given fill, on a 20Gi disk.
func platformVolumeSnapshot(fraction float64, volumes ...kitchenv1alpha1.PlatformVolumeStatus) *Snapshot {
	snapshot := newSnapshot()
	snapshot.Platform.Volumes = volumes
	for _, volume := range volumes {
		for _, claim := range volume.Claims {
			snapshot.VolumeUsage = append(snapshot.VolumeUsage, measured(claim, fraction))
		}
	}
	return snapshot
}

func measured(claim string, fraction float64) VolumeUsage {
	const capacity = 20 << 30
	return VolumeUsage{
		Namespace:     controller.PlatformNamespace,
		Claim:         claim,
		CapacityBytes: capacity,
		UsedBytes:     uint64(fraction * capacity),
		UsedFraction:  fraction,
	}
}

// The incident this rule was written for: the registry's volume read 85% on
// the Storage screen, pushes were already failing with ENOSPC, and nothing on
// the problems list said so.
func TestPlatformVolumeFillingWarnsAboutTheRegistryBeforeItRefusesPushes(t *testing.T) {
	snapshot := platformVolumeSnapshot(0.846, registryVolume())

	finding := expectOne(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
	if finding.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want warning", finding.Severity)
	}
	if finding.Fingerprint != "platform.volume-filling/kitchen-system/"+testRegistryClaim {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	if finding.Title != "image registry volume 85% full" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "on the image registry's volume "+testRegistryClaim)
	expectDetail(t, finding, "builds fail to push their images")
	expectDetail(t, finding, "Platform → Storage")
	expectDetail(t, finding, "POST /platform/storage/claims/"+testRegistryClaim+"/resize")
	expectDetail(t, finding, "registry.retention")
	if strings.HasSuffix(finding.Detail, "…") {
		t.Fatalf("detail was truncated, so its lever was lost: %s", finding.Detail)
	}
	if finding.Evidence != "/platform/storage?claim="+testRegistryClaim+"&namespace=kitchen-system" {
		t.Fatalf("evidence = %q, want the volume's row on the Storage screen", finding.Evidence)
	}
}

func TestPlatformVolumeFillingIsCriticalPastTheFullThreshold(t *testing.T) {
	snapshot := platformVolumeSnapshot(0.93, registryVolume())

	finding := expectOne(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
	if finding.Severity != SeverityCritical {
		t.Fatalf("severity = %q, want critical", finding.Severity)
	}
	if !strings.Contains(finding.Title, "93%") {
		t.Fatalf("title = %q, want the volume's fill", finding.Title)
	}
}

func TestPlatformVolumeFillingStaysQuietBelowTheWarning(t *testing.T) {
	snapshot := platformVolumeSnapshot(0.70, registryVolume())
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
}

// A volume nobody measured is not a full one. The rule judges the readings it
// has and is silent about the claim it has none for — the kubelet's group can
// answer for some volumes and not others, and inventing a fill for the rest
// would be a false alarm.
func TestPlatformVolumeFillingStaysQuietWithoutAReading(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Platform.Volumes = []kitchenv1alpha1.PlatformVolumeStatus{registryVolume()}
	snapshot.VolumeUsage = []VolumeUsage{{
		Namespace: controller.AppNamespace(testProject), Claim: testRegistryClaim, UsedFraction: 0.99,
		CapacityBytes: 1 << 30, UsedBytes: 1 << 30,
	}}
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))

	// A reading with no capacity behind it is no reading either.
	snapshot.VolumeUsage = []VolumeUsage{{
		Namespace: controller.PlatformNamespace, Claim: testRegistryClaim, UsedFraction: 0.99,
	}}
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
}

// A full claim in the platform namespace that no platform StatefulSet made is
// not a volume this platform grows, and pvc.filling already speaks for it.
func TestPlatformVolumeFillingJudgesOnlyThePlatformsOwnVolumes(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.VolumeUsage = []VolumeUsage{measured(testRegistryClaim, 0.99)}
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
}

// The store's disk is store.disk's, which knows the retention lever and how
// much of the disk is telemetry. A second platform finding on one disk would be
// one problem counted twice.
func TestPlatformVolumeFillingLeavesTheStoreToStoreDisk(t *testing.T) {
	store := kitchenv1alpha1.PlatformVolumeStatus{
		StatefulSet: "kitchen-clickhouse", Component: storeComponent, Claims: []string{testStoreClaim},
	}
	snapshot := platformVolumeSnapshot(0.95, store)
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))

	// Found by its claim as well as by its label, so a store whose workload
	// lost the label is still not judged twice.
	store.Component = ""
	snapshot = platformVolumeSnapshot(0.95, store)
	snapshot.Store.Claim = testStoreClaim
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
}

// Every platform volume is judged, and each says what stops when it is full.
func TestPlatformVolumeFillingNamesWhatEachVolumeIsFor(t *testing.T) {
	accounts := kitchenv1alpha1.PlatformVolumeStatus{
		StatefulSet: "kitchen-postgres", Component: "postgres", Claims: []string{"data-kitchen-postgres-0"},
	}
	unlabelled := kitchenv1alpha1.PlatformVolumeStatus{
		StatefulSet: "kitchen-something", Claims: []string{"data-kitchen-something-0"},
	}
	snapshot := platformVolumeSnapshot(0.9, accounts, unlabelled)

	findings := evaluate(t, SignalPlatformVolumeFilling, snapshot)
	if len(findings) != 2 {
		t.Fatalf("expected a finding per volume, got %d: %s", len(findings), describe(findings))
	}
	byClaim := map[string]Finding{}
	for _, finding := range findings {
		byClaim[finding.Scope.Name] = finding
	}
	expectDetail(t, byClaim["data-kitchen-postgres-0"], "signing in fails")
	// A component nobody described is still the platform's, and still full.
	expectDetail(t, byClaim["data-kitchen-something-0"], "used on the kitchen-something's volume")
	expectDetail(t, byClaim["data-kitchen-something-0"], "whatever writes to it fails")
}

// A volume on a storage class that admits no expansion cannot be grown, and a
// finding pointing at the resize would be pointing at a refusal.
func TestPlatformVolumeFillingDoesNotOfferAResizeTheClassRefuses(t *testing.T) {
	volume := registryVolume()
	expandable := false
	volume.Expandable, volume.StorageClass = &expandable, "local-path"
	snapshot := platformVolumeSnapshot(0.9, volume)

	finding := expectOne(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
	expectDetail(t, finding, "its storage class local-path does not allow expansion")
	if strings.Contains(finding.Detail, "/resize") {
		t.Fatalf("detail offers a resize the storage class refuses: %s", finding.Detail)
	}
	// Retention is still a lever when growing is not.
	expectDetail(t, finding, "registry.retention")
}

// The two inputs fail in the two ways the registry already handles: a
// singleton nobody could read is a rule that could not run, and volume stats
// nobody collects is a rule with nothing to say.
func TestPlatformVolumeFillingDegradesWithItsInputs(t *testing.T) {
	snapshot := platformVolumeSnapshot(0.95, registryVolume())
	snapshot.MarkUnreadable(InputKitchen, "kitchens.kitchen.bermos.dev \"kitchen\" is forbidden")
	finding := expectOne(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
	if finding.Severity != SeverityUnknown {
		t.Fatalf("severity = %q, want unknown", finding.Severity)
	}

	snapshot = platformVolumeSnapshot(0.95, registryVolume())
	snapshot.MarkNotApplicable(InputVolumeStats, "nothing reads the kubelet's volume stats")
	expectNone(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
}

// The whole path from the singleton's status to the finding: the gatherer is
// what hands the rule the platform's volumes, and a rule given none is silent.
func TestGatherHandsThePlatformsVolumesToTheRule(t *testing.T) {
	kitchen := kitchenSingleton(false)
	kitchen.Status.Storage = &kitchenv1alpha1.PlatformStorageStatus{
		Volumes: []kitchenv1alpha1.PlatformVolumeStatus{registryVolume()},
	}

	snapshot := Gather(context.Background(), Sources{
		Client:      testClient(t, kitchen),
		Store:       &stubStore{},
		VolumeUsage: measuredVolumes{measured(testRegistryClaim, 0.9)},
		Now:         func() time.Time { return testNow },
	}, Options{})

	if len(snapshot.Platform.Volumes) != 1 || snapshot.Platform.Volumes[0].StatefulSet != testRegistrySet {
		t.Fatalf("platform volumes = %+v, want the singleton's", snapshot.Platform.Volumes)
	}
	finding := expectOne(t, evaluate(t, SignalPlatformVolumeFilling, snapshot))
	if finding.Severity != SeverityCritical {
		t.Fatalf("severity = %q, want critical", finding.Severity)
	}
}

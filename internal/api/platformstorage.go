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

package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/retention"
	"github.com/Bermos/Kitchen/internal/signals"
)

// Storage: every claim the platform holds, what mounts it, and the health of
// the one database Kitchen runs itself.
//
// The two halves answer different failures. A claim that never bound is the
// classic first-install hang — no default StorageClass, which is one of the two
// prerequisites Kitchen keeps — and it is visible from the API server alone. A
// telemetry store quietly filling its disk is not visible from anywhere except
// its own system tables, and the day it stops accepting writes every screen in
// this section goes blank at once.
//
// How full each volume is comes from the kubelet's volume group, through the
// optional source the same gatherer reads it from. Where nothing satisfies that
// source, the rows carry no usage at all and the screen says why in a sentence,
// because "this volume is empty" and "nobody measured this volume" are not the
// same claim and must not draw the same bar.

// storeClaimMarker is what the bundled telemetry store's claim is named after.
// Every chart-generated name is release-name prefixed, so the claim is found by
// what its name contains rather than by an exact match — the same reason the
// component survey selects on a label.
const storeClaimMarker = "clickhouse"

// platformStorage answers the Storage screen.
func (s *Server) platformStorage(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claims := &corev1.PersistentVolumeClaimList{}
	if err := s.reader().List(ctx, claims); err != nil {
		s.writeError(w, err)
		return
	}
	pods := &corev1.PodList{}
	if err := s.reader().List(ctx, pods); err != nil {
		s.writeError(w, err)
		return
	}

	mounts := volumeMounts(pods.Items)
	usage, unmeasured := s.volumeUsage(ctx)
	body := platformStorageBody{
		Items: make([]volumeView, 0, len(claims.Items)),
		// Where nothing reads the kubelet's volume stats back out of the store,
		// every row's usage is unknown rather than zero, and the screen says so
		// once rather than drawing a hundred empty bars.
		UsageMessage: unmeasured,
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		key := claim.Namespace + "/" + claim.Name
		view := newVolumeView(claim, mounts[key])
		view.Usage = usage[key]
		body.Items = append(body.Items, view)
		body.Volumes++
		if !view.Bound {
			body.Unbound++
		}
		if view.Usage != nil && view.Usage.UsedFraction >= signals.VolumeFullFraction {
			body.Filling++
		}
	}
	sort.Slice(body.Items, func(i, j int) bool {
		if body.Items[i].Namespace != body.Items[j].Namespace {
			return body.Items[i].Namespace < body.Items[j].Namespace
		}
		return body.Items[i].Name < body.Items[j].Name
	})

	body.Store = s.storeHealth(ctx, claims.Items, usage, unmeasured)
	body.Flows = s.flowLoss()
	writeJSON(w, http.StatusOK, body)
}

// platformStorageBody is the Storage screen.
type platformStorageBody struct {
	Items   []volumeView `json:"items"`
	Volumes int          `json:"volumes"`
	Unbound int          `json:"unbound"`
	// Filling is how many are past the threshold the catalogue calls full. It
	// is a measured zero only when UsageMessage is empty.
	Filling int `json:"filling"`

	// Store is the telemetry store's own health, which is the platform's
	// storage problem rather than an application's.
	Store storeHealthView `json:"store"`
	// Flows is the follower's loss accounting: the other way ingest is lost,
	// counted where the store cannot see it.
	Flows *flowLossView `json:"flows,omitempty"`

	// UsageMessage says why no row carries a used figure.
	UsageMessage string `json:"usageMessage,omitempty"`
}

// volumeView is one PersistentVolumeClaim and what mounts it.
//
// It is called a volume rather than a claim throughout this screen because
// `/claims` already means something else in this API — a ResourceClaim, the
// platform's own kind for a provisioned database — and two things called claims
// in one dashboard is one too many.
type volumeView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Project attributes a claim to an application where the namespace says so.
	// The platform's own claims — the store's, the accounts database's — carry
	// none.
	Project string `json:"project,omitempty"`

	Phase        string `json:"phase"`
	Bound        bool   `json:"bound"`
	StorageClass string `json:"storageClass,omitempty"`
	Volume       string `json:"volume,omitempty"`
	// Requested is what the claim asked for and Capacity what it got, as the
	// API server spells them. They differ where a provisioner rounded up.
	Requested string `json:"requested,omitempty"`
	Capacity  string `json:"capacity,omitempty"`
	// Pods are the pods that mount it, which is what turns "this volume is
	// full" into something somebody can act on.
	Pods []string `json:"pods,omitempty"`
	// Usage is how full it is, where something reads the kubelet's volume
	// stats out of the store. Absent, with the body's UsageMessage saying why,
	// where nothing does.
	Usage *volumeUsageView `json:"usage,omitempty"`
	// Message is why an unbound claim is unbound. A claim Pending with no
	// storage class named is the missing-default-StorageClass install, which is
	// worth saying in those words.
	Message string `json:"message,omitempty"`
}

// volumeUsageView is how full one volume is, as the kubelet measured it.
type volumeUsageView struct {
	UsedBytes     uint64 `json:"usedBytes"`
	CapacityBytes uint64 `json:"capacityBytes"`
	// UsedFraction is 0..1, carried rather than recomputed so that a source
	// which knows the fraction more precisely than the two counts can say so.
	UsedFraction float64 `json:"usedFraction"`
}

// volumeUsage reads how full each claim is, or says why it did not.
//
// It is read as of now rather than over a window: the question a storage screen
// asks is how full a volume is, and the trend that turns that into a date is
// the disk-filling signal's, not this table's.
func (s *Server) volumeUsage(ctx context.Context) (map[string]*volumeUsageView, string) {
	source := s.volumeUsageSource(ctx)
	if source == nil {
		return nil, noVolumeUsageMessage
	}
	volumes, err := source.VolumeUsage(ctx, time.Now().UTC())
	if err != nil {
		s.log().Error(err, "the volume usage query failed")
		return nil, "how full each volume is could not be read, so usage is unknown rather than zero"
	}
	usage := make(map[string]*volumeUsageView, len(volumes))
	for _, volume := range volumes {
		usage[volume.Namespace+"/"+volume.Claim] = &volumeUsageView{
			UsedBytes:     volume.UsedBytes,
			CapacityBytes: volume.CapacityBytes,
			UsedFraction:  volume.UsedFraction,
		}
	}
	return usage, ""
}

// storeHealthView is the telemetry store's own state.
//
// Two numbers, and they answer two questions: how full the disk is, and how
// much of that is the telemetry's. They used to be one — the database's own
// parts over the claim's nominal capacity — which is a fraction of two
// different things, and it read 8.4% on a volume that was 89% full (#531).
type storeHealthView struct {
	// BytesOnDisk is what the telemetry itself occupies: the store's active
	// parts, which is the number retention governs. It is not the fill level —
	// anything else on the same volume is in neither it nor CapacityBytes.
	BytesOnDisk uint64 `json:"bytesOnDisk"`
	// CapacityBytes is the nominal size of the volume underneath, as the claim
	// reports it. Zero for an external store, where the platform does not own
	// the disk and has no business judging it.
	CapacityBytes uint64 `json:"capacityBytes,omitempty"`
	// Usage is how full that volume actually is, from the same kubelet stats
	// the table above draws — everything written to it, and the number
	// `store.disk` fires on. Absent, with UsageMessage saying why, where
	// nothing measured it.
	Usage *volumeUsageView `json:"usage,omitempty"`
	// UsageMessage says why Usage is missing, and is empty when it is not or
	// when there is no volume of the platform's to measure.
	UsageMessage string `json:"usageMessage,omitempty"`
	// UsedFraction is Usage.UsedFraction, kept flat because it is the field
	// this endpoint has always had and a reader of it should keep working. Its
	// *meaning* moved with this fix: it is how full the volume is, and no
	// longer the store's own parts over the claim's capacity. Absent, like
	// Usage, where nothing measured the disk — so a zero here is a measured
	// zero.
	UsedFraction float64 `json:"usedFraction,omitempty"`
	Claim        string  `json:"claim,omitempty"`
	// RowsPerSecond is the recent ingest rate across the tables the operator
	// writes and the collector fills. Zero while pods run is the store's own
	// stalled-ingest symptom.
	RowsPerSecond float64 `json:"rowsPerSecond"`
	// RetentionDays is the longest telemetry class's retention — the horizon
	// past which the store deliberately holds nothing at all. Retention is a
	// model of classes since #140 and this screen is about the disk, so
	// what it wants is the one that bounds it; `GET /platform/retention` has
	// the whole of it, class by class.
	RetentionDays int32 `json:"retentionDays,omitempty"`
	// Message says why the numbers are missing, and is empty when they are not.
	Message string `json:"message,omitempty"`
}

// newVolumeView reads one claim.
func newVolumeView(claim *corev1.PersistentVolumeClaim, pods []string) volumeView {
	view := volumeView{
		Namespace:    claim.Namespace,
		Name:         claim.Name,
		Phase:        string(claim.Status.Phase),
		Bound:        claim.Status.Phase == corev1.ClaimBound,
		Volume:       claim.Spec.VolumeName,
		Requested:    quantityString(claim.Spec.Resources.Requests, corev1.ResourceStorage),
		Capacity:     quantityString(claim.Status.Capacity, corev1.ResourceStorage),
		Pods:         pods,
		Project:      projectOfNamespace(claim.Namespace),
		StorageClass: storageClassOf(claim),
	}
	if !view.Bound {
		view.Message = "this claim is not bound, so nothing that needs it can start"
		if view.StorageClass == "" {
			view.Message += "; it names no storage class, so it is waiting for the cluster's default — " +
				"a cluster without one is the first-install hang Kitchen's prerequisites warn about"
		}
	}
	return view
}

func storageClassOf(claim *corev1.PersistentVolumeClaim) string {
	if claim.Spec.StorageClassName != nil {
		return *claim.Spec.StorageClassName
	}
	return ""
}

// volumeMounts is which pods mount which claim, keyed the way the claims are
// looked up.
func volumeMounts(pods []corev1.Pod) map[string][]string {
	mounts := map[string][]string{}
	for i := range pods {
		pod := &pods[i]
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim == nil {
				continue
			}
			key := pod.Namespace + "/" + volume.PersistentVolumeClaim.ClaimName
			mounts[key] = append(mounts[key], pod.Name)
		}
	}
	for key := range mounts {
		sort.Strings(mounts[key])
	}
	return mounts
}

// storeHealth reads the telemetry store's own size and ingest rate, and how
// full the volume it writes to is.
//
// The size is the store's own account of its active parts; the fill is the
// kubelet's account of the disk, the same reading every row of the table above
// carries, matched here to the store's claim. They are two numbers because the
// store is not the only thing that can fill that volume, and dividing the one
// by the other described neither (#531). The claim's nominal capacity comes
// from the API server, because ClickHouse knows how much it has written and
// nothing about the disk underneath.
//
// A failure here is a section with a message rather than a failed request: the
// claims above are the API server's and are still worth reading, and the store
// being unreachable is precisely when somebody opens this screen.
func (s *Server) storeHealth(
	ctx context.Context,
	claims []corev1.PersistentVolumeClaim,
	usage map[string]*volumeUsageView,
	unmeasured string,
) storeHealthView {
	view := storeHealthView{}
	// A claim of the platform's own that has not bound is named with no
	// capacity: an unbound claim and a store on somebody else's disk are
	// different answers, and only the second is an external store.
	switch capacity, name := storeClaim(claims); {
	case name == "":
	case capacity == 0:
		view.Claim = name
		view.UsageMessage = "this claim is not bound, so there is no volume to measure yet"
	default:
		view.CapacityBytes, view.Claim = capacity, name
		view.Usage = usage[controller.PlatformNamespace+"/"+name]
		if view.Usage != nil {
			view.UsedFraction = view.Usage.UsedFraction
		} else {
			// The same distinction the table draws: nothing measured this is
			// not the same claim as nothing is on it, and the two must not
			// render as one bar.
			view.UsageMessage = unmeasured
			if view.UsageMessage == "" {
				view.UsageMessage = "the kubelet has reported no volume stats for this claim, " +
					"so how full the disk is is unknown rather than zero"
			}
		}
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		view.RetentionDays = retention.Resolve(kitchen).LongestTelemetry()
	}

	store, err := s.logStore(ctx)
	if err != nil {
		if errors.Is(err, errNoLogStore) {
			view.Message = noStoreMessage
			return view
		}
		s.log().Error(err, "cannot reach the telemetry store for its own health")
		view.Message = "the telemetry store could not be reached, so its size and ingest rate are unknown"
		return view
	}
	// The same read the signals gatherer takes the store's health from, so the
	// screen and the `store.disk` finding can never disagree about the number —
	// and narrow, because these two figures are all either of them wants.
	stats, err := store.StoreStats(ctx)
	if err != nil {
		s.log().Error(err, "the store health query failed")
		view.Message = "the store's own size could not be read"
		return view
	}
	view.BytesOnDisk = stats.BytesOnDisk
	view.RowsPerSecond = stats.RowsPerSecond
	return view
}

// storeClaim finds the claim the bundled store writes to, and how big it is. An
// external store has no claim here at all, and answering no name is what tells
// the screen not to judge a disk the platform does not own.
//
// A claim that has not bound is still named, with a capacity of zero — the
// volume does not exist yet, which is not the same as the disk being somebody
// else's.
func storeClaim(claims []corev1.PersistentVolumeClaim) (uint64, string) {
	var largest uint64
	var name string
	for i := range claims {
		claim := &claims[i]
		if claim.Namespace != controller.PlatformNamespace ||
			!strings.Contains(claim.Name, storeClaimMarker) {
			continue
		}
		if capacity := quantityValue(claim.Status.Capacity.Storage()); name == "" || capacity > largest {
			largest, name = capacity, claim.Name
		}
	}
	return largest, name
}

// quantityValue reads a claim's reported capacity, which is absent until the
// volume is bound and negative never.
func quantityValue(quantity *resource.Quantity) uint64 {
	if quantity == nil {
		return 0
	}
	value := quantity.Value()
	if value < 0 {
		return 0
	}
	return uint64(value)
}

// projectOfNamespace is the application namespace's project, or nothing for a
// namespace the platform did not create for one. It is the inverse of
// controller.AppNamespace, and it is a prefix match because that is all the
// naming rule is.
func projectOfNamespace(namespace string) string {
	project, found := strings.CutPrefix(namespace, appNamespacePrefix)
	if !found || namespace == controller.PlatformNamespace {
		return ""
	}
	return project
}

// appNamespacePrefix is what controller.AppNamespace puts in front of a project
// name. It is spelled here rather than exported from there because this is the
// only place that reads the mapping backwards.
const appNamespacePrefix = "kitchen-"

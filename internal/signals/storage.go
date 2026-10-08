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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The storage table of §7: the volumes underneath, and the store the whole
// design depends on being able to write to.

const (
	SignalPVCPending    ID = "pvc.pending"
	SignalPVCFilling    ID = "pvc.filling"
	SignalAttachFailed  ID = "volume.attach-failed"
	SignalStoreDisk     ID = "store.disk"
	SignalIngestStalled ID = "store.ingest-stalled"
	SignalFlowsLost     ID = "ingest.flows-lost"
)

// SignalPlatformVolumeFilling is store.disk asked of the platform's other
// volumes: the registry's, the accounts database's, the object store's.
const SignalPlatformVolumeFilling ID = "platform.volume-filling"

// The event reasons the CSI path raises when a volume will not come up.
var attachFailureReasons = []string{"FailedAttachVolume", "FailedMount"}

func storageSignals() []Signal {
	return []Signal{{
		ID:       SignalPVCPending,
		Version:  2,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierTicket},
		Summary:  "a PersistentVolumeClaim is unbound — the classic first-install hang",
		Requires: []Input{InputClaims},
		Evaluate: evaluatePVCPending,
	}, {
		ID:      SignalPVCFilling,
		Version: 2,
		// Deliberately developer, where §7 lists it under an operator table.
		// A volume past 85% is scoped to the claim's project, and it is the
		// owning developer who fills it and who can delete something or ask
		// for more — unlike the two operator-audience rules beside it, which
		// are a missing default StorageClass and a misbehaving CSI driver.
		// Audience now drives ForEnvironment, so this line puts it on that
		// project's diagnostics strip rather than merely labelling it.
		Audience: AudienceDeveloper,
		Tiers:    Tiers{Developer: TierTicket, Operator: TierTicket},
		Summary:  "a volume is past 85% used",
		Requires: []Input{InputVolumeStats},
		Evaluate: evaluatePVCFilling,
	}, {
		ID:       SignalAttachFailed,
		Version:  2,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierTicket},
		Summary:  "the CSI driver could not attach or mount a volume",
		Requires: []Input{InputClusterEvents},
		Evaluate: evaluateAttachFailed,
	}, {
		ID: SignalStoreDisk,
		// Version 3: the fill it judges is the volume's, from the kubelet's
		// stats for the store's claim, rather than the `kitchen` database's
		// own parts over that claim's nominal capacity (#531).
		Version:  3,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierTicket},
		Summary:  "the volume the telemetry store writes to is filling",
		// The volume reading alone. The store's own size is a clause of the
		// detail, not a term of the judgement, so a `system.parts` read that
		// failed must not turn a disk this rule can see perfectly well into an
		// unknown.
		Requires: []Input{InputStoreVolume},
		Evaluate: evaluateStoreDisk,
	}, {
		ID:       SignalPlatformVolumeFilling,
		Version:  1,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierTicket},
		Summary:  "one of the platform's own volumes — the registry's, the accounts database's — is nearly full",
		// The singleton says which volumes are the platform's, and the
		// kubelet's stats say how full they are. A volume the stats carry no
		// row for is simply not judged: an absent reading is not a full disk.
		Requires: []Input{InputKitchen, InputVolumeStats},
		Evaluate: evaluatePlatformVolumeFilling,
	}, {
		ID:       SignalIngestStalled,
		Version:  2,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierTicket},
		Summary:  "nothing has been written to the store while pods are running",
		Requires: []Input{InputFreshness, InputPods},
		Evaluate: evaluateIngestStalled,
	}, {
		ID:       SignalFlowsLost,
		Version:  2,
		Audience: AudienceOperator,
		Tiers:    Tiers{Operator: TierLog},
		Summary:  "Hubble reported dropping events, so the request numbers under-report",
		Requires: []Input{InputIngest},
		Evaluate: evaluateFlowsLost,
	}}
}

// evaluatePVCPending names the suspect, because the reader's first install is
// exactly when they have least reason to guess it.
//
// A default StorageClass is one of the two things Kitchen keeps as a
// prerequisite rather than bundling — it has to exist before the cluster can
// run anything — and a cluster without one binds no claim at all. Every
// component that wants storage sits Pending, the pods sit Pending behind them,
// and nothing anywhere says the words "storage class".
func evaluatePVCPending(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	for i := range snapshot.Claims {
		claim := &snapshot.Claims[i]
		if claim.Status.Phase == corev1.ClaimBound || claim.DeletionTimestamp != nil {
			continue
		}
		scope := claimScope(claim.Namespace, claim.Name, snapshot)
		findings = append(findings, fire(SignalPVCPending, SeverityCritical, scope,
			claim.CreationTimestamp.Time,
			"storage is not bound",
			sentence(
				fmt.Sprintf("claim %s in namespace %s has been %s for %s",
					claim.Name, claim.Namespace, claimPhase(claim),
					duration(snapshot.Now.Sub(claim.CreationTimestamp.Time))),
				storageClassClause(claim),
				"a cluster with no default StorageClass binds nothing, and every pod waiting on the "+
					"claim stays Pending without ever saying so",
			),
			claimEvidence(claim.Namespace, claim.Name)))
	}
	return findings
}

func claimPhase(claim *corev1.PersistentVolumeClaim) string {
	if claim.Status.Phase == "" {
		return "unbound"
	}
	return strings.ToLower(string(claim.Status.Phase))
}

// storageClassClause says which class the claim asked for, since "none named,
// so the default" is the case that fails.
func storageClassClause(claim *corev1.PersistentVolumeClaim) string {
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName == "" {
		return "it names no StorageClass, so it needs the cluster's default one"
	}
	return fmt.Sprintf("it asks for StorageClass %q", *claim.Spec.StorageClassName)
}

func evaluatePVCFilling(snapshot *Snapshot) []Finding {
	findings := make([]Finding, 0, 1)
	volumes := append([]VolumeUsage(nil), snapshot.VolumeUsage...)
	sort.Slice(volumes, func(i, j int) bool {
		if volumes[i].Namespace != volumes[j].Namespace {
			return volumes[i].Namespace < volumes[j].Namespace
		}
		return volumes[i].Claim < volumes[j].Claim
	})

	for _, volume := range volumes {
		if volume.UsedFraction < VolumeFullFraction {
			continue
		}
		scope := Scope{Kind: ScopeVolume, Project: volume.Project, Name: volume.Claim}
		if volume.Project == "" {
			scope.Namespace = volume.Namespace
		}
		findings = append(findings, fire(SignalPVCFilling, SeverityWarning, scope, snapshot.Now,
			fmt.Sprintf("volume %s full", percent(volume.UsedFraction)),
			sentence(
				fmt.Sprintf("%s of %s used on claim %s",
					bytes(float64(volume.UsedBytes)), bytes(float64(volume.CapacityBytes)),
					volume.Claim),
				"nothing in the API server knows how full a volume is — this comes from the "+
					"kubelet's volume stats, and it is the only warning there will be",
				growableClause(volume.Namespace),
			),
			claimEvidence(volume.Namespace, volume.Claim)))
	}
	return findings
}

// growableClause names the lever, for the volumes that have one.
//
// Only the platform's own volumes do: growing one means rewriting the claim
// template of the StatefulSet that made it, and a project's claim was made by
// nothing here — `POST /platform/storage/claims/{name}/resize` does not find
// it. A finding that told a project's owner to go and grow their volume would
// be pointing at a button that is not there, which is the failure #533's
// screen exists to stop making.
func growableClause(namespace string) string {
	if namespace != controller.PlatformNamespace {
		return ""
	}
	return "whether this volume can be grown is on Platform → Storage, where this evidence lands"
}

func evaluateAttachFailed(snapshot *Snapshot) []Finding {
	// One finding per claim rather than per event: a volume that will not
	// mount raises the same warning every two minutes for as long as the pod
	// keeps being retried, and thirty rows say no more than one.
	byClaim := map[string][]int{}
	for i, event := range snapshot.ClusterEvents {
		if !matchesAny(event.Reason, attachFailureReasons) {
			continue
		}
		claim := claimFromMessage(event.Message)
		if claim == "" {
			claim = event.Name
		}
		key := event.Namespace + "/" + claim
		byClaim[key] = append(byClaim[key], i)
	}

	keys := make([]string, 0, len(byClaim))
	for key := range byClaim {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	findings := make([]Finding, 0, len(keys))
	for _, key := range keys {
		namespace, claim, _ := strings.Cut(key, "/")
		newest := snapshot.ClusterEvents[byClaim[key][0]]
		since := newest.Timestamp
		var occurrences uint32
		for _, index := range byClaim[key] {
			event := snapshot.ClusterEvents[index]
			occurrences += maxUint32(event.Count, 1)
			if event.Timestamp.After(newest.Timestamp) {
				newest = event
			}
			if event.Timestamp.Before(since) {
				since = event.Timestamp
			}
		}
		scope := claimScope(namespace, claim, snapshot)
		findings = append(findings, fire(SignalAttachFailed, SeverityCritical, scope, since,
			"volume will not mount",
			sentence(
				fmt.Sprintf("%s in %s", plural(int(occurrences), "failure", "failures"),
					duration(snapshot.Now.Sub(since))),
				withReason(newest.Reason, newest.Message),
				"the pod cannot start until the CSI driver attaches it, and the pod's own status "+
					"shows only that it is waiting",
			),
			eventsEvidence(namespace, "", claim)))
	}
	return findings
}

// claimFromMessage digs the claim's name out of a kubelet mount failure, whose
// message names the volume by its pod-spec name and the claim beside it. It
// returns empty when the message is not shaped that way, and the caller falls
// back to the involved object.
func claimFromMessage(message string) string {
	const marker = "volume with name "
	index := strings.Index(message, marker)
	if index < 0 {
		return ""
	}
	rest := message[index+len(marker):]
	name, _, _ := strings.Cut(rest, " ")
	return strings.Trim(name, `"`)
}

// evaluateStoreDisk judges the *volume*, not the database.
//
// The store's own size — the `kitchen` database's active parts — is what
// retention governs, and it is the wrong numerator for a fill level: anything
// else sharing the disk is in neither it nor the claim's nominal capacity, so
// the ratio of the two read 8.4% on a volume that was 89% full and the rule
// this platform has for exactly that outcome never fired (#531). The fill comes
// from the kubelet's stats for the store's claim, which is the same reading the
// storage table draws and which matches `df` inside the pod.
//
// The store's own size stays in the detail, because it is what says whether
// retention is the lever: a disk that is full of telemetry is a retention
// decision, and a disk that is full of something else is not. It is a clause
// and not a term of the judgement, which is why the rule requires the volume
// reading alone — a store that could not answer for itself does not stop this
// rule seeing a full disk.
func evaluateStoreDisk(snapshot *Snapshot) []Finding {
	store := snapshot.Store
	volume := store.Volume
	if volume == nil || volume.CapacityBytes == 0 {
		// Nothing measured the disk. The gatherer has marked
		// [InputStoreVolume] and the rule reports that it could not be
		// evaluated rather than reporting health it never read; this is the
		// belt to that pair of braces.
		return nil
	}
	if volume.UsedFraction < StoreDiskFraction {
		return nil
	}
	scope := Scope{Kind: ScopePlatform, Name: "store"}
	return []Finding{fire(SignalStoreDisk, SeverityCritical, scope, snapshot.Now,
		fmt.Sprintf("telemetry store's volume %s full", percent(volume.UsedFraction)),
		sentence(
			// "the store's volume" rather than "claim", because pvc.filling
			// says the latter about the same claim and two findings on one
			// screen should not open with the same words.
			fmt.Sprintf("%s of %s used on the store's volume %s", bytes(float64(volume.UsedBytes)),
				bytes(float64(volume.CapacityBytes)), volume.Claim),
			storeShareClause(store),
			retentionClause(snapshot),
			"a full store stops accepting writes, which takes logs, metrics and requests down "+
				"together and leaves every screen looking merely empty",
			// The other lever, and the one this finding used to name nowhere
			// (#533). Worded as where to look rather than as a promise: only
			// a volume on a storage class that admits expansion can be grown
			// at all, and that is a fact about the cluster this rule has no
			// reading of. The screen it points at does, and says so.
			"whether the volume itself can be grown is on Platform → Storage, where this evidence lands",
		),
		EvidencePlatformStorage)}
}

// storeShareClause says how much of the full disk the telemetry itself is,
// which is the difference between a retention problem and a lodger.
func storeShareClause(store StoreHealth) string {
	if store.BytesOnDisk == 0 {
		return ""
	}
	return fmt.Sprintf("the telemetry itself is %s of that", bytes(float64(store.BytesOnDisk)))
}

// retentionClause names the lever, since retention is the one thing that
// changes the store's size.
//
// It reports the *longest* telemetry class rather than a class each: a
// finding is one line, and the number worth putting in it is the one bounding
// the disk. The whole model is on `GET /platform/retention`.
func retentionClause(snapshot *Snapshot) string {
	if snapshot.Platform.RetentionDays <= 0 {
		return ""
	}
	return fmt.Sprintf("the longest telemetry retention is %d days", snapshot.Platform.RetentionDays)
}

// platformVolumeRole is what one of the platform's own volumes is for, in the
// words a finding about it needs: what is filling, what stops when it is full,
// and the lever other than growing it, where there is one.
type platformVolumeRole struct {
	name        string
	consequence string
	lever       string
}

// platformVolumeRoles are keyed by `app.kubernetes.io/component`, which is how
// status.storage.volumes names them. A volume of a component missing here is
// still judged — it is the platform's, and full is full — and says less about
// itself.
var platformVolumeRoles = map[string]platformVolumeRole{
	"registry": {
		name:        "image registry",
		consequence: "once it is full, builds fail to push their images and nothing new deploys",
		lever:       "or keep fewer images with the chart's registry.retention values (keepTags, keepPushedWithin)",
	},
	"postgres": {
		name:        "accounts database",
		consequence: "once it is full, Postgres stops accepting writes, and signing in fails with it",
	},
	"objectstore": {
		name:        "object store",
		consequence: "once it is full, every project's bucket refuses uploads",
	},
}

// storeComponent is the telemetry store's component, whose volume is
// store.disk's to judge — with the retention lever and the store's own share
// of the disk, which this rule knows nothing about. Judging it here as well
// would put two platform findings on one disk.
const storeComponent = "clickhouse"

// evaluatePlatformVolumeFilling is store.disk for every other volume the
// platform keeps for itself.
//
// pvc.filling already fires on these at 85%, as a warning about a claim. That
// is the wrong shape for them twice over. It comes too late: the bundled
// registry refused pushes with ENOSPC at a reading of 85% on a 20Gi volume,
// because a push needs room for every layer at once and the kubelet samples
// between pushes — so this warns from [PlatformVolumeWarnFraction]. And it says
// too little: a full registry is every project's builds failing to push, not
// one project's disk, so from [VolumeFullFraction] this one is critical and
// says what stops.
func evaluatePlatformVolumeFilling(snapshot *Snapshot) []Finding {
	usage := make(map[string]VolumeUsage, len(snapshot.VolumeUsage))
	for _, volume := range snapshot.VolumeUsage {
		if volume.Namespace == controller.PlatformNamespace {
			usage[volume.Claim] = volume
		}
	}

	findings := make([]Finding, 0, 1)
	for i := range snapshot.Platform.Volumes {
		platformVolume := &snapshot.Platform.Volumes[i]
		if platformVolume.Component == storeComponent {
			continue
		}
		claims := append([]string(nil), platformVolume.Claims...)
		sort.Strings(claims)
		for _, claim := range claims {
			if claim == snapshot.Store.Claim && claim != "" {
				continue
			}
			volume, measured := usage[claim]
			if !measured || volume.CapacityBytes == 0 || volume.UsedFraction < PlatformVolumeWarnFraction {
				continue
			}
			findings = append(findings, platformVolumeFinding(snapshot, platformVolume, volume))
		}
	}
	return findings
}

func platformVolumeFinding(
	snapshot *Snapshot,
	platformVolume *kitchenv1alpha1.PlatformVolumeStatus,
	volume VolumeUsage,
) Finding {
	role, known := platformVolumeRoles[platformVolume.Component]
	if !known {
		role = platformVolumeRole{
			name:        platformVolume.StatefulSet,
			consequence: "once it is full, whatever writes to it fails",
		}
	}
	severity := SeverityWarning
	if volume.UsedFraction >= VolumeFullFraction {
		severity = SeverityCritical
	}
	scope := Scope{Kind: ScopeVolume, Namespace: controller.PlatformNamespace, Name: volume.Claim}
	return fire(SignalPlatformVolumeFilling, severity, scope, snapshot.Now,
		fmt.Sprintf("%s volume %s full", role.name, percent(volume.UsedFraction)),
		sentence(
			// "the registry's volume" rather than "claim", because
			// pvc.filling says the latter about the same claim from 85%.
			fmt.Sprintf("%s of %s used on the %s's volume %s", bytes(float64(volume.UsedBytes)),
				bytes(float64(volume.CapacityBytes)), role.name, volume.Claim),
			role.consequence,
			growClause(platformVolume, volume.Claim),
			role.lever,
		),
		claimEvidence(controller.PlatformNamespace, volume.Claim))
}

// growClause names the lever every platform volume has, unless its storage
// class has said it does not: a volume on a class that admits no expansion
// can only be replaced, and a finding pointing at a resize that will be
// refused is pointing at a button that is greyed out.
func growClause(platformVolume *kitchenv1alpha1.PlatformVolumeStatus, claim string) string {
	if platformVolume.Expandable != nil && !*platformVolume.Expandable {
		class := "its storage class"
		if platformVolume.StorageClass != "" {
			class = fmt.Sprintf("its storage class %s", platformVolume.StorageClass)
		}
		return class + " does not allow expansion, so it cannot be grown in place"
	}
	return fmt.Sprintf("grow it on Platform → Storage (POST /platform/storage/claims/%s/resize)", claim)
}

// evaluateIngestStalled is node.silent asked of everybody at once.
//
// The "while pods run" half is what keeps it quiet on an idle cluster: a
// platform with nothing scheduled genuinely has nothing to say, and reporting
// its silence would be reporting that it is switched off.
func evaluateIngestStalled(snapshot *Snapshot) []Finding {
	if len(snapshot.Pods) == 0 {
		return nil
	}
	newest := snapshot.Store.NewestRow
	for _, lastSeen := range snapshot.Freshness {
		if lastSeen.After(newest) {
			newest = lastSeen
		}
	}
	silence := snapshot.Now.Sub(newest)
	if !newest.IsZero() && silence < IngestStalledAfter {
		return nil
	}

	scope := Scope{Kind: ScopePlatform, Name: "ingest"}
	return []Finding{fire(SignalIngestStalled, SeverityCritical, scope,
		ingestStalledSince(snapshot, newest),
		"nothing is reaching the store",
		sentence(
			ingestSilenceClause(silence, newest),
			fmt.Sprintf("%s are running and producing output", plural(len(snapshot.Pods), "pod", "pods")),
			"logs, metrics and requests all arrive through the same collector, so every screen goes "+
				"quiet together rather than one of them breaking",
		),
		EvidencePlatformStorage)}
}

func ingestSilenceClause(silence time.Duration, newest time.Time) string {
	if newest.IsZero() {
		return fmt.Sprintf("no row within the last %s", duration(FreshnessLookback))
	}
	return "newest row is " + duration(silence) + " old"
}

func ingestStalledSince(snapshot *Snapshot, newest time.Time) time.Time {
	if newest.IsZero() {
		return snapshot.Now.Add(-FreshnessLookback)
	}
	return newest
}

// evaluateFlowsLost is the one rule whose whole purpose is to contradict a
// number the platform is showing.
//
// Hubble drops events when a node's ring buffer overflows or the consumer
// lags, and Relay reports the drops in-stream. Nothing else notices: the
// request counts are simply lower than the traffic was, the charts are
// smooth, and every rate computed from them is wrong in the same direction.
func evaluateFlowsLost(snapshot *Snapshot) []Finding {
	ingest := snapshot.Ingest
	if ingest.FlowsLost < FlowsLostFiring && ingest.Reconnects == 0 {
		return nil
	}
	scope := Scope{Kind: ScopePlatform, Name: "flows"}
	return []Finding{fire(SignalFlowsLost, SeverityWarning, scope,
		flowsLostSince(snapshot, ingest),
		"request counts are under-reporting",
		sentence(
			flowsLostHeadline(ingest),
			"Hubble drops events when a node's buffer overflows or the follower lags; the rows that "+
				"survive are correct, there are simply fewer of them than there were requests",
			"raise hubble.eventBufferCapacity if this persists",
		),
		EvidencePlatformStorage)}
}

func flowsLostHeadline(ingest IngestHealth) string {
	clauses := make([]string, 0, 2)
	if ingest.FlowsLost > 0 {
		clauses = append(clauses, fmt.Sprintf("%d flow events lost in %s",
			ingest.FlowsLost, duration(ingest.Window)))
	}
	if ingest.Reconnects > 0 {
		clauses = append(clauses, fmt.Sprintf("%s, each leaving a gap of unknown size",
			plural(ingest.Reconnects, "stream reconnect", "stream reconnects")))
	}
	return strings.Join(clauses, " and ")
}

func flowsLostSince(snapshot *Snapshot, ingest IngestHealth) time.Time {
	if !ingest.LastLoss.IsZero() {
		return ingest.LastLoss
	}
	return snapshot.Now.Add(-ingest.Window)
}

// claimScope attributes a claim to the project whose namespace holds it, and
// to the platform namespace otherwise.
func claimScope(namespace, claim string, snapshot *Snapshot) Scope {
	if project := projectOfNamespace(snapshot, namespace); project != "" {
		return Scope{Kind: ScopeVolume, Project: project, Name: claim}
	}
	return Scope{Kind: ScopeVolume, Namespace: namespace, Name: claim}
}

// projectOfNamespace maps an application namespace back to its project, which
// only the operator's own naming convention can do.
func projectOfNamespace(snapshot *Snapshot, namespace string) string {
	for i := range snapshot.Environments {
		project := snapshot.Environments[i].Spec.ProjectRef.Name
		if controller.AppNamespace(project) == namespace {
			return project
		}
	}
	return ""
}

func matchesAny(value string, candidates []string) bool {
	for _, candidate := range candidates {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func maxUint32(value, floor uint32) uint32 {
	if value < floor {
		return floor
	}
	return value
}

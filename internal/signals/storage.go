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

// The event reasons the CSI path raises when a volume will not come up.
var attachFailureReasons = []string{"FailedAttachVolume", "FailedMount"}

func storageSignals() []Signal {
	return []Signal{{
		ID:       SignalPVCPending,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "a PersistentVolumeClaim is unbound — the classic first-install hang",
		Requires: []Input{InputClaims},
		Evaluate: evaluatePVCPending,
	}, {
		ID:      SignalPVCFilling,
		Version: 1,
		// Deliberately developer, where §7 lists it under an operator table.
		// A volume past 85% is scoped to the claim's project, and it is the
		// owning developer who fills it and who can delete something or ask
		// for more — unlike the two operator-audience rules beside it, which
		// are a missing default StorageClass and a misbehaving CSI driver.
		// Audience now drives ForEnvironment, so this line puts it on that
		// project's diagnostics strip rather than merely labelling it.
		Audience: AudienceDeveloper,
		Summary:  "a volume is past 85% used",
		Requires: []Input{InputVolumeStats},
		Evaluate: evaluatePVCFilling,
	}, {
		ID:       SignalAttachFailed,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "the CSI driver could not attach or mount a volume",
		Requires: []Input{InputClusterEvents},
		Evaluate: evaluateAttachFailed,
	}, {
		ID:       SignalStoreDisk,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "the telemetry store's own volume is filling",
		Requires: []Input{InputStore},
		Evaluate: evaluateStoreDisk,
	}, {
		ID:       SignalIngestStalled,
		Version:  1,
		Audience: AudienceOperator,
		Summary:  "nothing has been written to the store while pods are running",
		Requires: []Input{InputFreshness, InputPods},
		Evaluate: evaluateIngestStalled,
	}, {
		ID:       SignalFlowsLost,
		Version:  1,
		Audience: AudienceOperator,
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
			),
			claimEvidence(volume.Namespace, volume.Claim)))
	}
	return findings
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

func evaluateStoreDisk(snapshot *Snapshot) []Finding {
	store := snapshot.Store
	if store.CapacityBytes == 0 {
		// An external store's disk is not the platform's to judge, and a
		// percentage of an unknown capacity is not a number.
		return nil
	}
	used := float64(store.BytesOnDisk) / float64(store.CapacityBytes)
	if used < StoreDiskFraction {
		return nil
	}
	scope := Scope{Kind: ScopePlatform, Name: "store"}
	return []Finding{fire(SignalStoreDisk, SeverityCritical, scope, snapshot.Now,
		fmt.Sprintf("telemetry store %s full", percent(used)),
		sentence(
			fmt.Sprintf("%s of %s used", bytes(float64(store.BytesOnDisk)),
				bytes(float64(store.CapacityBytes))),
			retentionClause(snapshot),
			"a full store stops accepting writes, which takes logs, metrics and requests down "+
				"together and leaves every screen looking merely empty",
		),
		EvidencePlatformStorage)}
}

// retentionClause names the knob, since retention is the one lever that
// changes the store's size and it is a single number.
func retentionClause(snapshot *Snapshot) string {
	if snapshot.Platform.RetentionDays <= 0 {
		return ""
	}
	return fmt.Sprintf("retention is %d days", snapshot.Platform.RetentionDays)
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

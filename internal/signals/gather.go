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
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The impure half. Everything that talks to the API server, the store or the
// network happens here, once, and produces the value the rules read.
//
// Gather never returns an error, and that is the design rather than a
// shortcut. Partial knowledge is the normal state of this evaluation — a store
// that is down, a CRD that is not installed, a resolver that timed out — and a
// gatherer that failed whole would take every rule down with the one input
// that broke. So each read either fills its part of the snapshot or marks its
// input, and the registry turns the marks into findings that say what could not
// be seen.

// Sources is where a snapshot comes from.
type Sources struct {
	// Client reads the API server. In the operator it is the manager's cached
	// client, which is what makes thirty-odd rules over the cluster's objects
	// cost nothing per evaluation.
	Client client.Client

	// Store is the telemetry store. Nil means none is configured — an
	// installation without observability wired up — and every store-backed
	// rule is skipped rather than reported as broken.
	Store Store

	// HostMetrics, VolumeUsage and Ingest are the three sources §7 names that
	// nothing satisfies yet; see their interfaces. Nil marks their inputs
	// not-applicable.
	HostMetrics HostMetricsSource
	VolumeUsage VolumeUsageSource
	Ingest      IngestAccounting

	// Resolver resolves published names for dns.mismatch. Nil means no
	// probing, which is not a fault.
	Resolver Resolver

	// Now overrides the clock. Tests set it; the operator does not.
	Now func() time.Time
}

// Options narrows a gather.
type Options struct {
	// Project and Environment narrow the per-environment store reads to one
	// environment, which is what the diagnostics strip needs: a screen about
	// one preview should not cost a query per environment on the platform.
	//
	// The API-server reads stay whole either way. They come from a cache that
	// already holds everything, so narrowing them would save nothing and would
	// hide the cluster-wide conditions — a saturated node, an unprogrammed
	// Gateway — that are the most useful thing to tell a developer whose
	// environment is misbehaving.
	Project     string
	Environment string

	// Concurrency bounds the per-environment store reads. Zero takes the
	// default.
	Concurrency int
}

// defaultConcurrency is how many per-environment store reads run at once. Four
// keeps a platform-wide evaluation from serialising over dozens of
// environments without turning one screen refresh into a load test of a
// single-node ClickHouse.
const defaultConcurrency = 4

// Gather builds a snapshot for the catalogue to read.
func Gather(ctx context.Context, sources Sources, options Options) *Snapshot {
	now := time.Now().UTC()
	if sources.Now != nil {
		now = sources.Now().UTC()
	}
	snapshot := &Snapshot{
		Now: now,
		// The default rather than the zero value, and set before anything is
		// read: an installation whose singleton could not be fetched still
		// has to be judged against *some* clock, and a correlation threshold
		// of zero would call every pair of failures an incident. gatherKitchen
		// replaces it with the configured one where it can.
		Policy:    DefaultPolicy(),
		Traffic:   map[EnvKey]clickhouse.RequestSeries{},
		Resources: map[EnvKey]clickhouse.ResourceSeries{},
		Freshness: map[string]time.Time{},
		NodeUsage: map[string]NodeUsage{},
		OpenedAt:  map[string]time.Time{},
	}

	gatherCluster(ctx, sources, snapshot)
	gatherStore(ctx, sources, snapshot, options)
	gatherDNS(ctx, sources, snapshot)
	// The timeline is joined from four sources that are read in three places,
	// so it is ordered once here rather than by whichever leg wrote last.
	SortPlatformChanges(snapshot.PlatformChanges)
	return snapshot
}

// gatherCluster reads everything the API server knows.
func gatherCluster(ctx context.Context, sources Sources, snapshot *Snapshot) {
	if sources.Client == nil {
		for _, input := range []Input{
			InputPods, InputWorkloads, InputNodes, InputClaims, InputGateways,
			InputRoutes, InputCertificates, InputEnvironments, InputProjects,
			InputBuilds, InputKitchen, InputResourceClaims, InputPlatformChanges,
		} {
			snapshot.MarkUnreadable(input, "no API server client is configured")
		}
		return
	}

	gatherKitchen(ctx, sources.Client, snapshot)
	gatherWorkloads(ctx, sources.Client, snapshot)
	gatherEdgeObjects(ctx, sources.Client, snapshot)

	pods := &corev1.PodList{}
	listInto(ctx, sources.Client, snapshot, InputPods, pods, func() {
		snapshot.Pods = pods.Items
	})

	nodes := &corev1.NodeList{}
	listInto(ctx, sources.Client, snapshot, InputNodes, nodes, func() {
		snapshot.Nodes = nodes.Items
	})

	claims := &corev1.PersistentVolumeClaimList{}
	listInto(ctx, sources.Client, snapshot, InputClaims, claims, func() {
		snapshot.Claims = claims.Items
	})

	environments := &kitchenv1alpha1.EnvironmentList{}
	listInto(ctx, sources.Client, snapshot, InputEnvironments, environments, func() {
		snapshot.Environments = environments.Items
	})

	projects := &kitchenv1alpha1.ProjectList{}
	listInto(ctx, sources.Client, snapshot, InputProjects, projects, func() {
		snapshot.Projects = projects.Items
	})

	builds := &kitchenv1alpha1.BuildList{}
	listInto(ctx, sources.Client, snapshot, InputBuilds, builds, func() {
		snapshot.Builds = builds.Items
	})

	claimsAttached := &kitchenv1alpha1.ResourceClaimList{}
	listInto(ctx, sources.Client, snapshot, InputResourceClaims, claimsAttached, func() {
		snapshot.ResourceClaims = claimsAttached.Items
	})

	gatherPlatformChanges(ctx, sources.Client, snapshot)

	// The designations are folded once, here, from whichever of the two
	// lists came back. A rule that could not have both is skipped by its
	// Requires before it ever reads the map.
	snapshot.Continuity = ContinuityFacts(snapshot.Projects, snapshot.Environments)
}

// gatherKitchen reads the singleton, which carries the configuration the rules
// need before they can judge anything — and the component survey, which
// platform.component-unhealthy folds in rather than re-deriving.
func gatherKitchen(ctx context.Context, reader client.Client, snapshot *Snapshot) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := reader.Get(ctx, key, kitchen); err != nil {
		snapshot.MarkUnreadable(InputKitchen, err.Error())
		return
	}
	// The thresholds this round is judged against, resolved once. Every
	// finding records them; see [Finding.Policy].
	snapshot.Policy = PolicyFrom(kitchen)
	snapshot.Platform = PlatformFacts{
		BaseDomain:         kitchen.Spec.BaseDomain,
		GatewayAddress:     kitchen.Status.GatewayAddress,
		PublicAddresses:    kitchen.Spec.Ingress.PublicAddresses,
		CloudflaredEnabled: kitchen.Spec.Ingress.Cloudflared.Enabled,
		AuditLog:           kitchen.Spec.Compliance.Audit.Enabled,
		Components:         kitchen.Status.Components,
		RetentionDays:      retention.Resolve(kitchen).LongestTelemetry(),
	}
}

// gatherPlatformChanges joins the two kinds that record what the platform did
// to itself into rung 3's timeline: releases and addon upgrades.
//
// Both are kept after they finish on purpose — "the list is the installation's
// upgrade history" is PlatformUpdate's own comment, and AddonUpgrade was built
// to its shape (#474) — which is the whole reason this join is possible at all.
// An attempt that is still running is on the timeline as well as one that
// finished: an upgrade in flight at the moment six projects started failing is
// the most interesting row this list can hold.
func gatherPlatformChanges(ctx context.Context, reader client.Client, snapshot *Snapshot) {
	updates := &kitchenv1alpha1.PlatformUpdateList{}
	upgrades := &kitchenv1alpha1.AddonUpgradeList{}
	for _, list := range []client.ObjectList{updates, upgrades} {
		if err := reader.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) {
				snapshot.MarkNotApplicable(InputPlatformChanges,
					"the platform's own change kinds are not installed in this cluster")
				return
			}
			// Half a timeline is worse than none: a ladder that could see
			// releases and not addon upgrades would climb to rung 3 for one
			// and stop at rung 2 for the other, which reads as a difference
			// between the incidents rather than between the reads.
			snapshot.MarkUnreadable(InputPlatformChanges, err.Error())
			return
		}
	}

	changes := make([]PlatformChange, 0, len(updates.Items)+len(upgrades.Items))
	for i := range updates.Items {
		update := &updates.Items[i]
		at := platformChangeAt(update.Status.StartedAt, update.CreationTimestamp)
		changes = append(changes, PlatformChange{
			At:   at,
			Kind: "release",
			Summary: fmt.Sprintf("this platform was upgraded to %s (%s)",
				update.Spec.Version, phaseOrRunning(string(update.Status.Phase))),
		})
	}
	for i := range upgrades.Items {
		upgrade := &upgrades.Items[i]
		at := platformChangeAt(upgrade.Status.StartedAt, upgrade.CreationTimestamp)
		changes = append(changes, PlatformChange{
			At:   at,
			Kind: "addon",
			Summary: fmt.Sprintf("the %s addon was upgraded (%s)",
				upgrade.Spec.Addon, phaseOrRunning(string(upgrade.Status.Phase))),
		})
	}
	snapshot.PlatformChanges = append(snapshot.PlatformChanges, changes...)
}

// platformChangeAt prefers the change's own instant over the record's. A
// PlatformUpdate and an AddonUpgrade both stamp when the install job was
// created rather than when the object was written, for exactly this reason —
// the timestamp has to be the platform change's and not the observer's — and
// the creation time is the fallback for a record that has not started yet.
func platformChangeAt(started *metav1.Time, created metav1.Time) time.Time {
	if started != nil && !started.IsZero() {
		return started.Time.UTC()
	}
	return created.Time.UTC()
}

// phaseOrRunning is a phase a reader can read. An empty phase is an attempt
// nothing has reported on, which is "in flight" rather than a blank.
func phaseOrRunning(phase string) string {
	if phase == "" {
		return "in flight"
	}
	return strings.ToLower(phase)
}

func gatherWorkloads(ctx context.Context, reader client.Client, snapshot *Snapshot) {
	deployments := &appsv1.DeploymentList{}
	statefulSets := &appsv1.StatefulSetList{}
	daemonSets := &appsv1.DaemonSetList{}
	// Jobs are listed with the three serving kinds, and for the same reason:
	// a Job whose pods are refused at admission has no pods at all, which is
	// the one failure that looks like health from every other angle. Which of
	// them the survey judges is decided in snapshotJobs, not here — a list
	// narrowed by a selector could not tell "the platform created no Jobs"
	// from "the read came back empty".
	jobs := &batchv1.JobList{}
	for _, list := range []client.ObjectList{deployments, statefulSets, daemonSets, jobs} {
		if err := reader.List(ctx, list); err != nil {
			// One kind failing makes the whole workload picture wrong rather
			// than partial: admission-refused turns on "this workload has no
			// pods", and a list that came back empty because it failed looks
			// exactly like a workload that wants nothing.
			snapshot.MarkUnreadable(InputWorkloads, err.Error())
			return
		}
	}
	snapshot.Deployments = deployments.Items
	snapshot.StatefulSets = statefulSets.Items
	snapshot.DaemonSets = daemonSets.Items
	snapshot.Jobs = jobs.Items
}

// gatherEdgeObjects reads the Gateway API kinds and cert-manager's.
func gatherEdgeObjects(ctx context.Context, reader client.Client, snapshot *Snapshot) {
	gateways := &gatewayv1.GatewayList{}
	listInto(ctx, reader, snapshot, InputGateways, gateways, func() {
		snapshot.Gateways = gateways.Items
	})

	routes := &gatewayv1.HTTPRouteList{}
	listInto(ctx, reader, snapshot, InputRoutes, routes, func() {
		snapshot.Routes = routes.Items
	})

	gatherCertificates(ctx, reader, snapshot)
}

// certificateGVK addresses cert-manager's kind as an unstructured object
// rather than through its Go types, which is how the operator addresses all of
// them — it keeps the build off cert-manager's release cadence.
var certificateGVK = schema.GroupVersionKind{
	Group:   "cert-manager.io",
	Version: "v1",
	Kind:    "CertificateList",
}

func gatherCertificates(ctx context.Context, reader client.Client, snapshot *Snapshot) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(certificateGVK)
	if err := reader.List(ctx, list); err != nil {
		// No CRD means cert-manager is not installed, which is a supported
		// configuration (tls.mode none, or a certificate supplied by hand).
		// There is nothing to report and nothing broken.
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			snapshot.MarkNotApplicable(InputCertificates,
				"cert-manager's Certificate kind is not installed")
			return
		}
		snapshot.MarkUnreadable(InputCertificates, err.Error())
		return
	}
	snapshot.Certificates = make([]Certificate, 0, len(list.Items))
	for i := range list.Items {
		snapshot.Certificates = append(snapshot.Certificates, readCertificate(&list.Items[i]))
	}
}

// readCertificate lifts the fields the catalogue reads out of the
// unstructured object, once, so that no rule has to dig through nested maps.
func readCertificate(object *unstructured.Unstructured) Certificate {
	certificate := Certificate{Namespace: object.GetNamespace(), Name: object.GetName()}

	names, _, _ := unstructured.NestedStringSlice(object.Object, "spec", "dnsNames")
	certificate.DNSNames = names
	certificate.NotAfter = nestedTime(object, "status", "notAfter")
	certificate.RenewalTime = nestedTime(object, "status", "renewalTime")

	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		conditionType, _ := condition["type"].(string)
		status, _ := condition["status"].(string)
		reason, _ := condition["reason"].(string)
		message, _ := condition["message"].(string)
		switch conditionType {
		case "Ready":
			certificate.Ready = status == "True"
			certificate.Reason, certificate.Message = reason, message
		case "Issuing":
			// A renewal in progress reports itself here while Ready stays true
			// on the still-valid old certificate, so this is the only place a
			// stuck renewal says so.
			if status == "True" {
				certificate.IssuingMessage = withReason(reason, message)
			}
		}
	}
	return certificate
}

func nestedTime(object *unstructured.Unstructured, fields ...string) time.Time {
	value, found, err := unstructured.NestedString(object.Object, fields...)
	if !found || err != nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// listInto runs one list and marks its input on failure, which is the shape
// every API-server read here has.
func listInto(
	ctx context.Context,
	reader client.Client,
	snapshot *Snapshot,
	input Input,
	list client.ObjectList,
	keep func(),
) {
	if err := reader.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) {
			snapshot.MarkNotApplicable(input, "the kind is not installed in this cluster")
			return
		}
		snapshot.MarkUnreadable(input, err.Error())
		return
	}
	keep()
}

// gatherStore reads the telemetry store.
func gatherStore(ctx context.Context, sources Sources, snapshot *Snapshot, options Options) {
	gatherOptionalSources(ctx, sources, snapshot)

	if sources.Store == nil {
		for _, input := range []Input{
			InputRawRequests, InputRequests, InputResources,
			InputClusterEvents, InputFreshness, InputStore, InputAudit, InputHistory,
		} {
			snapshot.MarkNotApplicable(input, "no telemetry store is configured")
		}
		return
	}

	gatherClusterEvents(ctx, sources.Store, snapshot)
	gatherAuditChanges(ctx, sources.Store, snapshot)
	gatherHistory(ctx, sources.Store, snapshot)
	gatherFreshness(ctx, sources.Store, snapshot)
	gatherStoreHealth(ctx, sources.Store, snapshot)
	gatherTraffic(ctx, sources.Store, snapshot, options)
}

// gatherOptionalSources fills the three inputs §7 names that nothing satisfies
// yet. A nil source is not-applicable rather than unreadable: the rule is dark
// because nothing writes its input, which is a gap in the platform and not a
// fault in this cluster.
func gatherOptionalSources(ctx context.Context, sources Sources, snapshot *Snapshot) {
	if sources.HostMetrics == nil {
		snapshot.MarkNotApplicable(InputHostMetrics,
			"nothing reads host_metrics back out of the store yet")
	} else if usage, err := sources.HostMetrics.NodeUsage(ctx,
		snapshot.Now.Add(-ResourceWindow), snapshot.Now, ResourceBucket); err != nil {
		snapshot.MarkUnreadable(InputHostMetrics, err.Error())
	} else {
		for _, node := range usage {
			snapshot.NodeUsage[node.Node] = node
		}
	}

	if sources.VolumeUsage == nil {
		snapshot.MarkNotApplicable(InputVolumeStats,
			"nothing reads the kubelet's volume stats back out of the store yet")
	} else if volumes, err := sources.VolumeUsage.VolumeUsage(ctx, snapshot.Now); err != nil {
		snapshot.MarkUnreadable(InputVolumeStats, err.Error())
	} else {
		snapshot.VolumeUsage = volumes
	}

	if sources.Ingest == nil {
		snapshot.MarkNotApplicable(InputIngest,
			"the flow follower does not report its loss accounting yet")
	} else if health, err := sources.Ingest.IngestHealth(ctx); err != nil {
		snapshot.MarkUnreadable(InputIngest, err.Error())
	} else {
		snapshot.Ingest = health
	}
}

func gatherClusterEvents(ctx context.Context, store Store, snapshot *Snapshot) {
	events, err := store.QueryK8sEvents(ctx, clickhouse.K8sEventQuery{
		Since: snapshot.Now.Add(-ResourceWindow),
		Until: snapshot.Now,
		Limit: clickhouse.MaxK8sEventLimit,
	})
	if err != nil {
		snapshot.MarkUnreadable(InputClusterEvents, err.Error())
		return
	}
	snapshot.ClusterEvents = events
	snapshot.PlatformChanges = append(snapshot.PlatformChanges, infrastructureChanges(events)...)
}

// infrastructureChanges is the third leg of rung 3's timeline: the things the
// cluster did that nobody in this platform asked for.
//
// A node coming back and a Gateway being reprogrammed are the two that explain
// several projects failing at one instant, and both arrive as ordinary
// Kubernetes events. The reasons are matched rather than the kinds alone,
// because every pod that ever failed to schedule is also an event on a node.
func infrastructureChanges(events []clickhouse.K8sEvent) []PlatformChange {
	changes := make([]PlatformChange, 0, 2)
	for _, event := range events {
		kind, ok := infrastructureChangeKinds[strings.ToLower(event.Kind)+"/"+strings.ToLower(event.Reason)]
		if !ok {
			continue
		}
		where := event.Name
		if where == "" {
			where = event.Node
		}
		changes = append(changes, PlatformChange{
			At:      event.Timestamp.UTC(),
			Kind:    kind,
			Summary: fmt.Sprintf("%s %s: %s", kind, where, event.Reason),
		})
	}
	return changes
}

// infrastructureChangeKinds is the small, named set of (kind, reason) pairs
// that count as the platform changing underneath its tenants. It is a list
// rather than a prefix match because "every Warning event on a Node" is most
// of the events a busy cluster produces, and a timeline of those would raise
// every correlation to rung 3 and mean nothing.
var infrastructureChangeKinds = map[string]string{
	"node/rebooted":                  "node",
	"node/nodenotready":              "node",
	"node/nodeready":                 "node",
	"node/nodenotschedulable":        "node",
	"node/nodeallocatableenforced":   "node",
	"gateway/programmed":             "gateway",
	"gateway/reconciled":             "gateway",
	"gateway/addressnotassigned":     "gateway",
	"gateway/listenersnotvalid":      "gateway",
	"gateway/listenersnotprogrammed": "gateway",
}

// gatherAuditChanges is the fourth leg: what a person did to this platform.
//
// Only the privileged records, which is the audit log's own word for a
// transition that moved a *control* rather than a workload — a waiver, a
// requirement, a credential, a grant, a settings change. A deploy is not a
// platform change: it is one project's, it is on that project's timeline, and
// a correlation across six projects that named it would be naming the loudest
// row rather than the cause.
func gatherAuditChanges(ctx context.Context, store Store, snapshot *Snapshot) {
	// An installation that keeps no audit log has no table to read, because
	// the table is the compliance reconcile's and it never ran. Asking anyway
	// answers UNKNOWN_TABLE, which would put `audit_records` on the
	// singleton's `status.signals.unreadable` and on the operator's screen
	// every round, for ever, describing a setting as a fault. The API's own
	// reads ask the same question first — see openAuditLog and #441.
	//
	// It is asked only where the singleton was actually read: a platform
	// whose configuration could not be fetched is one this cannot answer for,
	// and guessing "off" would silently stop consulting a log that exists.
	if snapshot.Available(InputKitchen) && !snapshot.Platform.AuditLog {
		snapshot.MarkNotApplicable(InputAudit,
			"this installation keeps no audit log, so there is none to correlate against")
		return
	}
	records, err := store.QueryAuditRecords(ctx, clickhouse.AuditQuery{
		Privileged: true,
		Since:      snapshot.Now.Add(-ResourceWindow),
		Until:      snapshot.Now,
		Limit:      clickhouse.DefaultAuditLimit,
	})
	if err != nil {
		snapshot.MarkUnreadable(InputAudit, err.Error())
		return
	}
	for _, record := range records {
		snapshot.PlatformChanges = append(snapshot.PlatformChanges, PlatformChange{
			At:   record.Timestamp.UTC(),
			Kind: "operator",
			Summary: fmt.Sprintf("%s changed %s %s", record.Actor,
				strings.ToLower(record.Kind), record.Name),
		})
	}
}

// gatherHistory reads when each open condition was first seen, which is the
// only clock the correlation ladder can honestly use.
//
// An installation whose signals schema has never been created has no history
// rather than an unreadable one — the table arrives with the Kitchen
// reconcile, and a round on a platform that has not had one yet is not
// blind, it is early. Anything else is a read that failed, and the ladder
// says so rather than treating a round's worth of identically stamped
// findings as simultaneous.
func gatherHistory(ctx context.Context, store Store, snapshot *Snapshot) {
	open, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		if clickhouse.IsUnknownTable(err) {
			snapshot.MarkNotApplicable(InputHistory,
				"nothing has been recorded yet, so no condition has a known start")
			return
		}
		snapshot.MarkUnreadable(InputHistory, err.Error())
		return
	}
	for _, row := range open {
		opened := row.OpenedAt
		if opened.IsZero() {
			opened = row.At
		}
		if opened.IsZero() {
			continue
		}
		// One condition is up to two rows and they open together; the
		// earliest is the condition's own start either way.
		if was, seen := snapshot.OpenedAt[row.Fingerprint]; !seen || opened.Before(was) {
			snapshot.OpenedAt[row.Fingerprint] = opened.UTC()
		}
	}
}

func gatherFreshness(ctx context.Context, store Store, snapshot *Snapshot) {
	freshness, err := store.TelemetryFreshness(ctx, FreshnessLookback)
	if err != nil {
		snapshot.MarkUnreadable(InputFreshness, err.Error())
		return
	}
	for _, node := range freshness {
		snapshot.Freshness[node.Node] = node.LastSeen
		if node.LastSeen.After(snapshot.Store.NewestRow) {
			snapshot.Store.NewestRow = node.LastSeen
		}
	}
}

// gatherStoreHealth reads the store's own size, and the size of the volume it
// writes to — which comes from the API server, because ClickHouse knows how
// much it has written and nothing about the disk underneath.
//
// It asks for exactly those two numbers. This runs on every evaluation of the
// catalogue, which is every platform screen and every environment's diagnostics
// strip, so a read that also aggregated a day of logs and events here would be
// work the whole platform paid for and nothing read.
func gatherStoreHealth(ctx context.Context, store Store, snapshot *Snapshot) {
	stats, err := store.StoreStats(ctx)
	if err != nil {
		snapshot.MarkUnreadable(InputStore, err.Error())
		return
	}
	snapshot.Store.BytesOnDisk = stats.BytesOnDisk
	snapshot.Store.RowsPerSecond = stats.RowsPerSecond
	snapshot.Store.CapacityBytes = storeCapacity(snapshot)
}

// storeCapacity finds the claim the bundled ClickHouse writes to. An external
// store has no claim here, and its capacity stays zero — which store.disk
// reads as "not the platform's disk to judge".
func storeCapacity(snapshot *Snapshot) uint64 {
	var largest uint64
	for i := range snapshot.Claims {
		claim := &snapshot.Claims[i]
		if claim.Namespace != controller.PlatformNamespace ||
			!strings.Contains(claim.Name, storeClaimMarker) {
			continue
		}
		if capacity := quantityValue(claim.Status.Capacity.Storage()); capacity > largest {
			largest = capacity
		}
	}
	return largest
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

// storeClaimMarker is what the telemetry store's claim is named after. Every
// name the chart generates is release-name prefixed, so the claim is found by
// what it contains rather than by an exact name — the same reason the component
// survey selects on a label instead of on names.
const storeClaimMarker = "clickhouse"

func gatherTraffic(ctx context.Context, store Store, snapshot *Snapshot, options Options) {
	gatherProjectTraffic(ctx, store, snapshot)
	gatherUnroutedHosts(ctx, store, snapshot)
	gatherEnvironmentSeries(ctx, store, snapshot, options)
}

func gatherProjectTraffic(ctx context.Context, store Store, snapshot *Snapshot) {
	recentStart := snapshot.Now.Add(-RecentWindow)
	recent, err := store.ProjectTraffic(ctx, clickhouse.ProjectTrafficQuery{
		Since: recentStart,
		Until: snapshot.Now,
	})
	if err != nil {
		snapshot.MarkUnreadable(InputRequests, err.Error())
		return
	}
	baseline, err := store.ProjectTraffic(ctx, clickhouse.ProjectTrafficQuery{
		Since: recentStart.Add(-BaselineWindow),
		Until: recentStart,
	})
	if err != nil {
		snapshot.MarkUnreadable(InputRequests, err.Error())
		return
	}
	snapshot.ProjectTrafficRecent = recent
	snapshot.ProjectTrafficBaseline = baseline
}

// gatherUnroutedHosts reads the bucket of hosts nobody published, which is a
// group-by over the raw request rows rather than over the minute rollup —
// unattributed traffic is exactly what the rollup's project key cannot carry.
// So a failure here marks the raw table's input and leaves the rollup's rules
// alone.
func gatherUnroutedHosts(ctx context.Context, store Store, snapshot *Snapshot) {
	hosts, err := store.UnroutedHosts(ctx, clickhouse.PlatformRequestsQuery{
		Since: snapshot.Now.Add(-UnroutedWindow),
		Until: snapshot.Now,
	})
	if err != nil {
		snapshot.MarkUnreadable(InputRawRequests, err.Error())
		return
	}
	snapshot.UnroutedHosts = hosts
}

// gatherEnvironmentSeries reads the two per-environment series.
//
// They are the only reads that scale with the number of environments, so they
// are the only ones bounded by a worker pool — and the only ones an Options
// narrowing applies to. A failure on any environment marks the whole input:
// half a picture of the platform's traffic would let the rules report on the
// environments that answered and silently omit the ones that did not, which is
// exactly the shape of dishonesty this package exists to avoid.
func gatherEnvironmentSeries(ctx context.Context, store Store, snapshot *Snapshot, options Options) {
	keys := selectedKeys(snapshot, options)
	if len(keys) == 0 {
		return
	}

	concurrency := options.Concurrency
	if concurrency < 1 {
		concurrency = defaultConcurrency
	}

	var mutex sync.Mutex
	var wait sync.WaitGroup
	var trafficErr, resourceErr error
	slots := make(chan struct{}, concurrency)

	for _, key := range keys {
		wait.Add(1)
		go func(key EnvKey) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			traffic, tErr := store.RequestSeries(ctx, clickhouse.RequestSeriesQuery{
				RequestQuery: clickhouse.RequestQuery{
					Project:     key.Project,
					Environment: key.Environment,
					Since:       snapshot.Now.Add(-TrafficWindow),
					Until:       snapshot.Now,
				},
				Buckets: bucketCount(TrafficWindow, TrafficBucket),
			})
			resources, rErr := store.ResourceSeries(ctx, clickhouse.ResourceSeriesQuery{
				Project:     key.Project,
				Environment: key.Environment,
				Since:       snapshot.Now.Add(-ResourceWindow),
				Until:       snapshot.Now,
				Buckets:     bucketCount(ResourceWindow, ResourceBucket),
			})

			mutex.Lock()
			defer mutex.Unlock()
			if tErr != nil {
				trafficErr = tErr
			} else {
				snapshot.Traffic[key] = traffic
			}
			if rErr != nil {
				resourceErr = rErr
			} else {
				snapshot.Resources[key] = resources
			}
		}(key)
	}
	wait.Wait()

	if trafficErr != nil {
		snapshot.MarkUnreadable(InputRequests, trafficErr.Error())
	}
	if resourceErr != nil {
		snapshot.MarkUnreadable(InputResources, resourceErr.Error())
	}
}

// selectedKeys is the environments this gather reads series for.
func selectedKeys(snapshot *Snapshot, options Options) []EnvKey {
	keys := snapshot.EnvKeys()
	if options.Environment == "" {
		return keys
	}
	narrowed := make([]EnvKey, 0, 1)
	for _, key := range keys {
		if key.Environment != options.Environment {
			continue
		}
		if options.Project != "" && key.Project != options.Project {
			continue
		}
		narrowed = append(narrowed, key)
	}
	return narrowed
}

// bucketCount asks for a resolution: how many buckets of `width` fit the
// window.
//
// The extra bucket is load-bearing. Both readers round the requested width up
// to the next rung of their own ladder, so asking for exactly as many buckets
// as fit puts the wanted width *on* a rung and any rounding error above it —
// which lands on the next rung up and halves the resolution. One more bucket
// than fits keeps it below. Neither reader is handed a number past its own
// ceiling here, and both clamp anyway.
func bucketCount(window, width time.Duration) int {
	return int(window/width) + 1
}

// gatherDNS resolves a sample of the names the platform published.
//
// Three things decide whether this runs at all, and each of them is the
// difference between a useful signal and a permanent false one:
//
//   - Behind cloudflared, names point at Cloudflare's edge by design. There is
//     nothing to compare against and a mismatch is the correct configuration.
//   - With neither a Gateway address nor a declared public one there is
//     nothing to probe on behalf of: no answer could be judged, and no answer
//     could even be judged missing on the platform's behalf.
//   - A resolver that is itself broken must not look like broken DNS. A name
//     that does not exist is a finding; a lookup that timed out or was refused
//     is an input that could not be read, and one of those poisons the whole
//     sample rather than contributing a false mismatch to it.
func gatherDNS(ctx context.Context, sources Sources, snapshot *Snapshot) {
	switch {
	case sources.Resolver == nil:
		snapshot.MarkNotApplicable(InputDNS, "no resolver is configured")
		return
	case snapshot.Platform.CloudflaredEnabled:
		snapshot.MarkNotApplicable(InputDNS,
			"cloudflared is enabled, so published names point at Cloudflare rather than at the Gateway")
		return
	case snapshot.Platform.GatewayAddress == "" && len(snapshot.Platform.PublicAddresses) == 0:
		snapshot.MarkNotApplicable(InputDNS,
			"the Gateway has no address yet, so there is nothing for a name to resolve to")
		return
	}

	probes := make([]DNSProbe, 0, DNSProbeLimit)
	for _, candidate := range publishedHosts(snapshot) {
		addresses, err := sources.Resolver.LookupHost(ctx, candidate.Host)
		switch {
		case err == nil:
			candidate.Addresses, candidate.Exists = addresses, true
		case isNotFound(err):
			candidate.Exists = false
		default:
			snapshot.MarkUnreadable(InputDNS,
				fmt.Sprintf("resolving %s failed: %s", candidate.Host, err.Error()))
			return
		}
		probes = append(probes, candidate)
	}
	snapshot.DNS = probes
}

// isNotFound tells "this name has no record" from "the resolver misbehaved".
// Only the first is a finding.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

// publishedHosts samples the names the platform published, capped, in a stable
// order so that two evaluations probe the same names and produce the same
// fingerprints.
//
// The sample is deliberately small. What this rule is looking for is systemic —
// the wildcard record missing, or pointing at the old address — and if the
// wildcard is wrong the first name proves it. Walking every hostname would turn
// one screen refresh into a hundred lookups to answer the same question.
func publishedHosts(snapshot *Snapshot) []DNSProbe {
	probes := make([]DNSProbe, 0, len(snapshot.Environments))
	for i := range snapshot.Environments {
		env := &snapshot.Environments[i]
		host := hostOf(env.Status.URL)
		if host == "" {
			continue
		}
		probes = append(probes, DNSProbe{
			Host:        host,
			Project:     env.Spec.ProjectRef.Name,
			Environment: env.Name,
		})
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].Host < probes[j].Host })
	if len(probes) > DNSProbeLimit {
		probes = probes[:DNSProbeLimit]
	}
	return probes
}

// hostOf takes the hostname out of a published URL, port and all removed.
func hostOf(published string) string {
	if published == "" {
		return ""
	}
	parsed, err := url.Parse(published)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Hostname()
}

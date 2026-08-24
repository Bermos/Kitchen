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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// EnvKey identifies one environment across the snapshot's maps. It is the pair
// the store is keyed on, because the store knows projects and environments and
// not the object names either of them happens to have.
type EnvKey struct {
	Project     string
	Environment string
}

// Snapshot is everything the catalogue reads, gathered once.
//
// It is a value, not a set of handles: by the time a rule sees it, every read
// has already happened and failed or succeeded. That is what makes the rules
// pure, and it is also what makes them testable — every test in this package
// builds one of these as a struct literal and calls a rule directly.
//
// Fields left at their zero value are simply empty inputs, which is not the
// same as unreadable ones; see [Snapshot.MarkUnreadable]. A snapshot with no
// pods and the pods input available means a cluster running nothing, and the
// rules are right to be quiet about it.
type Snapshot struct {
	// Now is the instant the snapshot was taken. Nothing in this package reads
	// the wall clock; every "in the last ten minutes" is relative to this, so a
	// test can place a condition wherever it likes.
	Now time.Time

	// Platform is what the Kitchen singleton says about itself.
	Platform PlatformFacts

	// From the API server.
	Pods         []corev1.Pod
	Deployments  []appsv1.Deployment
	StatefulSets []appsv1.StatefulSet
	DaemonSets   []appsv1.DaemonSet
	Nodes        []corev1.Node
	Claims       []corev1.PersistentVolumeClaim
	Gateways     []gatewayv1.Gateway
	Routes       []gatewayv1.HTTPRoute
	Certificates []Certificate
	Environments []kitchenv1alpha1.Environment
	Projects     []kitchenv1alpha1.Project
	Builds       []kitchenv1alpha1.Build

	// Continuity is each environment's resolved criticality and disruption
	// tolerances (#141), folded from Projects and Environments once by
	// [ContinuityFacts]. It is a derived field on the snapshot rather than
	// something the rules re-derive, because the resolution rule — production
	// falls back to its project, a preview never does — has exactly one right
	// answer and thirty-odd chances to be got differently wrong.
	Continuity map[EnvKey]ContinuityFor

	// ClusterEvents is the recent Warning history, newest first, as the
	// k8s_events recorder wrote it.
	ClusterEvents []clickhouse.K8sEvent

	// Traffic is per environment, bucketed across the recent window *and* the
	// trailing baseline it is compared against — one series rather than two
	// queries, since a baseline is just the older end of the same read.
	Traffic map[EnvKey]clickhouse.RequestSeries

	// Resources is per environment: CPU and memory against their limits,
	// restarts and OOM kills, from metrics_5m.
	Resources map[EnvKey]clickhouse.ResourceSeries

	// ProjectTrafficRecent and ProjectTrafficBaseline are the same question
	// asked of two windows, which is the only way a percentile comparison is
	// honest: p95 does not average, so "p95 now versus p95 then" has to be two
	// merges rather than one series folded in half.
	ProjectTrafficRecent   []clickhouse.ProjectTraffic
	ProjectTrafficBaseline []clickhouse.ProjectTraffic

	// UnroutedHosts is the edge's bucket of hosts the platform never published.
	UnroutedHosts []clickhouse.UnroutedHost

	// Freshness is when the store last saw a row from each node's collector. A
	// node absent from this map reported nothing within the lookback — which is
	// the contract of the query behind it, and the whole point of node.silent.
	Freshness map[string]time.Time

	// NodeUsage is host saturation per node.
	NodeUsage map[string]NodeUsage

	// VolumeUsage is how full each mounted claim is.
	VolumeUsage []VolumeUsage

	// Store is the telemetry store's own health.
	Store StoreHealth

	// Ingest is the flow follower's self-accounting.
	Ingest IngestHealth

	// DNS is the result of the operator's own resolution of a sample of
	// published names.
	DNS []DNSProbe

	// inputs records what could and could not be read. Unexported so that the
	// only way to mark an input is through the two methods below, which make
	// the caller say why.
	inputs map[Input]inputRecord
}

// PlatformFacts is what the Kitchen singleton contributes: the configuration a
// rule has to know before it can judge anything.
type PlatformFacts struct {
	// BaseDomain is the suffix every generated URL sits under.
	BaseDomain string
	// GatewayAddress is the shared Gateway's programmed address, empty until
	// it has one.
	GatewayAddress string
	// CloudflaredEnabled changes what "correct DNS" means: names point at
	// Cloudflare's edge rather than at the Gateway, so dns.mismatch has nothing
	// to compare against.
	CloudflaredEnabled bool
	// Components is the component survey, as the Kitchen reconciler last wrote
	// it. platform.component-unhealthy folds it into the same feed rather than
	// re-deriving it.
	Components []kitchenv1alpha1.ComponentStatus
	// RetentionDays is the store's configured retention, which is what the
	// store's disk usage should be read against.
	RetentionDays int32
}

// Certificate is the part of a cert-manager Certificate the catalogue reads.
//
// It is a plain struct because cert-manager's kinds are addressed as
// unstructured objects — the operator does that everywhere, to avoid tying the
// build to cert-manager's release cadence — and digging through nested maps is
// not something thirty-odd rules should each learn to do. [Gather] does it
// once.
type Certificate struct {
	Namespace string
	Name      string
	DNSNames  []string
	// NotAfter is when the certificate stops being valid; zero when it has
	// never been issued.
	NotAfter time.Time
	// RenewalTime is when cert-manager intends to renew it.
	RenewalTime time.Time
	// Ready is the Ready condition; Reason and Message are its explanation,
	// which for a stuck ACME order is the verbatim error the CA returned and is
	// the single most useful string on the certificates table.
	Ready   bool
	Reason  string
	Message string
	// IssuingMessage is the Issuing condition's message where there is one: a
	// renewal in progress that keeps failing reports its reason there rather
	// than on Ready, which stays true on the still-valid old certificate.
	IssuingMessage string
}

// NodeUsage is one node's saturation, bucketed over the snapshot's window.
type NodeUsage struct {
	Node string
	// CPU and Memory are utilisation fractions in 0..1, oldest bucket first.
	CPU    []Bucket
	Memory []Bucket
	// Filesystems is one entry per mount point kept by the collector's
	// exclusion list — the node's real disks, not its thousand image layers.
	Filesystems []NodeFilesystem
	// BucketWidth is how wide one bucket is, which is what turns a run of
	// buckets into a duration.
	BucketWidth time.Duration
}

// NodeFilesystem is one mounted filesystem's fill over time.
type NodeFilesystem struct {
	MountPoint    string
	Device        string
	CapacityBytes uint64
	// Used is the used fraction in 0..1, oldest bucket first. The projection
	// behind node.disk-filling is a straight line through these.
	Used []Bucket
}

// VolumeUsage is how full one PersistentVolumeClaim is, from the kubelet's
// volume stats.
type VolumeUsage struct {
	Namespace     string
	Claim         string
	Project       string
	Environment   string
	CapacityBytes uint64
	UsedBytes     uint64
	// UsedFraction is 0..1, carried rather than recomputed so that a source
	// that knows the fraction more precisely than the two byte counts can say
	// so.
	UsedFraction float64
}

// StoreHealth is the telemetry store's own state.
type StoreHealth struct {
	// BytesOnDisk is what its active parts occupy.
	BytesOnDisk uint64
	// CapacityBytes is the size of the volume underneath, read from the claim
	// in the platform namespace. Zero for an external store, where the platform
	// does not own the disk and has no business judging it.
	CapacityBytes uint64
	// RowsPerSecond is the recent ingest rate across the tables the operator
	// writes and the collector fills.
	RowsPerSecond float64
	// NewestRow is the most recent telemetry the store holds, over every node.
	// Zero means nothing arrived within the freshness lookback at all, which is
	// the strongest form of stalled there is.
	NewestRow time.Time
}

// IngestHealth is the flow follower's accounting of what it lost.
//
// Hubble reports LostEvent notices in-stream when a node's ring buffer
// overflows or the consumer lags. Counting them is what lets the platform say
// "request counts under-report" instead of quietly showing fewer requests than
// happened.
type IngestHealth struct {
	// Window is how far back the counts reach.
	Window time.Duration
	// FlowsLost is the number of events Relay reported dropping.
	FlowsLost uint64
	// Reconnects is how many times the follower had to re-establish the
	// stream, each of which is a gap of unknown size.
	Reconnects int
	// LastLoss is when the most recent loss was reported.
	LastLoss time.Time
}

// DNSProbe is one resolution the operator performed itself.
type DNSProbe struct {
	Host string
	// Addresses is what the name resolved to, empty when it did not exist.
	Addresses []string
	// Exists distinguishes "this name has no record" from "this name resolves
	// somewhere else". Both are mismatches; only one of them is a missing
	// record, and the detail says which.
	Exists bool
	// Project and Environment attribute a generated URL back to what published
	// it, so the finding can link at the environment rather than at a hostname.
	Project     string
	Environment string
}

// inputState is what the gatherer managed to do about one input.
type inputState int

const (
	// inputAvailable: it was read. An empty result is a real answer.
	inputAvailable inputState = iota
	// inputUnreadable: the read failed. Rules that need it report that they
	// could not be evaluated.
	inputUnreadable
	// inputNotApplicable: the question does not arise in this installation —
	// no store configured, DNS behind a tunnel, a source nothing writes yet.
	// Rules that need it produce nothing at all.
	inputNotApplicable
)

type inputRecord struct {
	state  inputState
	reason string
}

// MarkUnreadable records that an input could not be read, and why. Rules that
// require it will report that they could not be evaluated rather than
// reporting health they did not measure.
func (s *Snapshot) MarkUnreadable(input Input, reason string) {
	s.mark(input, inputRecord{state: inputUnreadable, reason: reason})
}

// MarkNotApplicable records that an input does not exist in this installation,
// which is not a fault. Rules that require it are skipped silently.
func (s *Snapshot) MarkNotApplicable(input Input, reason string) {
	s.mark(input, inputRecord{state: inputNotApplicable, reason: reason})
}

func (s *Snapshot) mark(input Input, record inputRecord) {
	if s.inputs == nil {
		s.inputs = map[Input]inputRecord{}
	}
	s.inputs[input] = record
}

// Available reports whether an input was read. An input nothing ever marked
// counts as available: a snapshot built by hand in a test is a snapshot of a
// world where everything was readable, and making every test declare that
// would only add ceremony.
func (s *Snapshot) Available(input Input) bool {
	state, _ := s.inputState(input)
	return state == inputAvailable
}

func (s *Snapshot) inputState(input Input) (inputState, string) {
	record, marked := s.inputs[input]
	if !marked {
		return inputAvailable, ""
	}
	return record.state, record.reason
}

// Unreadable lists the inputs that could not be read, with their reasons, in
// name order. It is how a health strip says "the telemetry store is
// unreachable" once, above a list that would otherwise repeat it per rule.
func (s *Snapshot) Unreadable() []InputFailure {
	failures := make([]InputFailure, 0, len(s.inputs))
	for input, record := range s.inputs {
		if record.state == inputUnreadable {
			failures = append(failures, InputFailure{Input: input, Reason: record.reason})
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Input < failures[j].Input })
	return failures
}

// InputFailure is one input the snapshot could not read.
type InputFailure struct {
	Input  Input  `json:"input"`
	Reason string `json:"reason"`
}

// EnvKeys lists the snapshot's environments in a fixed order, so that a rule
// walking them produces the same round twice.
func (s *Snapshot) EnvKeys() []EnvKey {
	keys := make([]EnvKey, 0, len(s.Environments))
	for i := range s.Environments {
		env := &s.Environments[i]
		keys = append(keys, EnvKey{Project: env.Spec.ProjectRef.Name, Environment: env.Name})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Project != keys[j].Project {
			return keys[i].Project < keys[j].Project
		}
		return keys[i].Environment < keys[j].Environment
	})
	return keys
}

// PodsFor is one environment's pods, found the way everything else in the
// operator finds them: by the labels the reconciler put on them.
func (s *Snapshot) PodsFor(key EnvKey) []corev1.Pod {
	pods := make([]corev1.Pod, 0, 4)
	for i := range s.Pods {
		pod := &s.Pods[i]
		if pod.Labels[controller.LabelProject] == key.Project &&
			pod.Labels[controller.LabelEnvironment] == key.Environment {
			pods = append(pods, *pod)
		}
	}
	return pods
}

// ReadyPods counts the pods of an environment that are actually serving. It is
// deliberately not the Deployment's ready-replica count: a Deployment scaled to
// zero by the autoscaler reports what it was asked for, and the question
// env.no-backend asks is whether anything is there to answer.
func (s *Snapshot) ReadyPods(key EnvKey) int {
	ready := 0
	for _, pod := range s.PodsFor(key) {
		if podReady(&pod) {
			ready++
		}
	}
	return ready
}

// EventsFor keeps the cluster events matching a reason and an involved object.
// An empty reason or name matches anything, which is what a rule wanting "every
// FailedMount in this namespace" asks for.
func (s *Snapshot) EventsFor(namespace, kind, name, reason string) []clickhouse.K8sEvent {
	matched := make([]clickhouse.K8sEvent, 0, 4)
	for _, event := range s.ClusterEvents {
		switch {
		case namespace != "" && event.Namespace != namespace,
			kind != "" && event.Kind != kind,
			name != "" && event.Name != name,
			reason != "" && !strings.EqualFold(event.Reason, reason):
			continue
		}
		matched = append(matched, event)
	}
	return matched
}

// podReady reads the one condition that says a pod is serving traffic.
func podReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for i := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[i]
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

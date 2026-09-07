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
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The problems list and the diagnostics strip: one catalogue, one snapshot,
// narrowed differently. The operator asks what is wrong with the platform; the
// developer asks what is wrong with their environment, and gets the subset of
// the same round that is about it.
//
// There are two ways to answer, and they are the same shape on purpose. Where
// the operator's background evaluation loop is running, these endpoints read
// what it recorded: the conditions `signal_transitions` says are open, which
// carry when the platform first saw them rather than only what the objects can
// prove about themselves. Where it is not — switched off, no telemetry store
// to record into, or a round has not landed recently enough to be current — a
// screen asks, the gatherer reads the API server and the store once, and the
// thirty-odd rules run over the value it produced.
//
// The fallback is not a transitional courtesy. It is what makes the loop safe
// to switch off and safe to be behind: an empty problems list because nothing
// has been recorded yet would be the strongest claim this platform makes,
// made about nothing.
//
// The interesting half of this file is the degradation. A snapshot is normally
// partial: the store may be down, a CRD may not be installed, a resolver may
// have timed out. The signals package models that as three states per input —
// read, unreadable, does not arise — and the one thing the API must never do is
// flatten them into an empty list, because an empty problems list is the
// strongest claim this platform makes.

// FlowFollower is the flow collector as this API reads it: its accounting of
// what Hubble reported losing. *flows.Collector satisfies it, and the interface
// exists so the API's tests do not need a Relay connection to answer.
type FlowFollower interface {
	Loss(window time.Duration) flows.Loss
}

// dnsLookupTimeout bounds one name resolution. The DNS rule probes a handful of
// published names on every platform screen load, and a resolver that is itself
// unreachable would otherwise hold the request open for the resolv.conf
// timeout multiplied by the probe limit. A lookup that runs out of time is an
// input that could not be read, which is not the same as a name that does not
// exist — see [signals.SystemResolver].
const dnsLookupTimeout = 2 * time.Second

// environmentSignals answers the environment page's diagnostics strip: what is
// currently wrong with one environment, worst first, each with its evidence.
func (s *Server) environmentSignals(w http.ResponseWriter, req *http.Request) {
	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	ctx := req.Context()
	project := env.Spec.ProjectRef.Name

	if recorded, ok := s.recordedSignals(ctx); ok {
		body := recorded.body(recorded.findings.ForEnvironment(project, env.Name))
		body.Project, body.Environment = project, env.Name
		writeJSON(w, http.StatusOK, body)
		return
	}

	// The narrowing is the store reads', not the cluster's: it saves a query
	// per environment on the platform, which is the whole reason a strip about
	// one preview can be rendered on every page load.
	snapshot := signals.Gather(ctx, s.signalSources(ctx), signals.Options{
		Project:     project,
		Environment: env.Name,
	})
	findings := signals.Catalogue().Evaluate(snapshot).ForEnvironment(project, env.Name)

	body := newSignalsBody(findings, snapshot)
	body.Project, body.Environment = project, env.Name
	writeJSON(w, http.StatusOK, body)
}

// platformSignals answers the operator's problems list: every finding that is
// currently firing anywhere on the platform, worst first.
//
// This screen is the inbox docs/OBSERVABILITY.md §7 designs. Where the loop is
// running it *is* reading the recorded round; where it is not it evaluates one
// here, in the same shape, so that the screen never depends on detection being
// switched on.
func (s *Server) platformSignals(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	if recorded, ok := s.recordedSignals(ctx); ok {
		// Firing drops the unknowns for the same reason the evaluated path
		// does — except that a recorded round has none: a rule that could not
		// be evaluated is not a condition, so the loop never wrote one. What
		// it could not read is in `unreadable`, off the round's own report.
		writeJSON(w, http.StatusOK, recorded.body(recorded.findings.Firing()))
		return
	}

	snapshot := signals.Gather(ctx, s.signalSources(ctx), signals.Options{})
	// Firing drops the rules that could not be evaluated. They are not lost:
	// `unreadable` names each failed input once, above a list that would
	// otherwise repeat the same store outage thirty times.
	findings := signals.Catalogue().Evaluate(snapshot).Firing()

	writeJSON(w, http.StatusOK, newSignalsBody(findings, snapshot))
}

// signalsBody is one evaluated round.
type signalsBody struct {
	// Items are the findings, worst first. A finding carries its own scope, so
	// one shape serves both screens.
	Items signals.Findings `json:"items"`
	// Counts is the headline — "2 problems" — so that a strip does not have to
	// count client-side to decide whether to render at all.
	Counts findingCounts `json:"counts"`
	// Unreadable is every input the gather could not read, with the reason,
	// once. It is what keeps an empty Items list honest: no findings because
	// nothing is wrong, and no findings because nothing could be read, are
	// different answers and this is where they differ.
	Unreadable []signals.InputFailure `json:"unreadable,omitempty"`
	// EvaluatedAt is the instant the round was taken — the snapshot's, when
	// this request evaluated one, and the loop's last round when the answer
	// came out of the recorded history. It is how a screen says how fresh the
	// answer is, and it is the field that moves when detection stops.
	EvaluatedAt time.Time `json:"evaluatedAt"`

	// Source says which of the two answers this is: `evaluated` for a round
	// taken to serve this request, `recorded` for one the background loop
	// took and wrote down. It is served rather than inferred because the two
	// differ in a way a reader can act on — a recorded finding's history is
	// in the store and an evaluated one's is nowhere — and because "detection
	// is not running" is worth being able to see from a screen.
	Source string `json:"source"`

	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// findingCounts is a round by severity. Unknown is deliberately absent: a rule
// that could not be evaluated is in Unreadable, not in the count of problems.
type findingCounts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

func newSignalsBody(findings signals.Findings, snapshot *signals.Snapshot) signalsBody {
	return signalsRound(findings, snapshot.Unreadable(), snapshot.Now, sourceEvaluated)
}

// The two answers, named. They are values on the wire, so they are constants
// here rather than literals in two handlers.
const (
	sourceEvaluated = "evaluated"
	sourceRecorded  = "recorded"
)

// signalsRound is one answer, however it was arrived at: the findings, what
// could not be read, and when.
func signalsRound(
	findings signals.Findings,
	unreadable []signals.InputFailure,
	at time.Time,
	source string,
) signalsBody {
	body := signalsBody{
		Items:       itemsOf(findings),
		Unreadable:  unreadable,
		EvaluatedAt: at,
		Source:      source,
	}
	for _, finding := range findings {
		switch finding.Severity {
		case signals.SeverityCritical:
			body.Counts.Critical++
		case signals.SeverityWarning:
			body.Counts.Warning++
		case signals.SeverityInfo:
			body.Counts.Info++
		case signals.SeverityUnknown:
			// Counted nowhere on purpose; Unreadable says it once instead.
		}
	}
	if len(body.Unreadable) == 0 {
		// Omitted rather than empty: the field is a list of things that went
		// wrong, and an empty one on every healthy answer would be noise the
		// reader learns to skip past.
		body.Unreadable = nil
	}
	return body
}

// recordedRound is the background loop's last round as these endpoints answer
// from it: the conditions it holds open, the inputs it could not read, and
// when it ran.
type recordedRound struct {
	findings   signals.Findings
	unreadable []signals.InputFailure
	at         time.Time
}

// body is the recorded round narrowed to what one screen renders.
func (r recordedRound) body(findings signals.Findings) signalsBody {
	return signalsRound(findings, r.unreadable, r.at, sourceRecorded)
}

// staleRounds is how many intervals the recorded history may be behind before
// these endpoints stop answering from it.
//
// Three, because a round is allowed to be slow and a leader is allowed to
// change: one missed round is a store query that took longer than usual, and
// three in a row is a loop that has stopped. Past it the endpoints evaluate
// instead, which costs a screen a gather and costs the reader nothing —
// whereas serving a history that stopped moving an hour ago would answer "what
// is wrong right now" with what was wrong an hour ago, and say `evaluatedAt`
// quietly enough that nobody notices.
const staleRounds = 3

// recordedSignals is the loop's last round, or nothing when the history cannot
// answer for the platform's current state.
//
// Four things have to hold, and each of them is a way the recorded answer
// would otherwise be worse than an evaluated one: the loop has completed a
// round, that round is recent enough to be about now, the store it wrote to is
// readable, and the read succeeded. Any of them failing is a fallback rather
// than an error — there is a correct answer available, it just costs a gather.
func (s *Server) recordedSignals(ctx context.Context) (recordedRound, bool) {
	status, ok := s.currentRound(ctx)
	if !ok {
		return recordedRound{}, false
	}

	store, err := s.logStore(ctx)
	if err != nil {
		return recordedRound{}, false
	}
	rows, err := store.OpenSignalTransitions(ctx)
	if err != nil {
		return recordedRound{}, false
	}

	round := recordedRound{
		findings: signals.TransitionsFindings(signals.TransitionsFrom(rows)),
		at:       status.LastEvaluated.Time,
	}
	for _, failure := range status.Unreadable {
		round.unreadable = append(round.unreadable, signals.InputFailure{
			Input:  signals.Input(failure.Input),
			Reason: failure.Reason,
		})
	}
	return round, true
}

// currentRound is the first two of those four: the loop has completed a round,
// and it is recent enough to be about now.
//
// It is split out because three callers want exactly that question and not the
// findings behind it — the alerts endpoints, which read the transitions
// themselves rather than as findings, and the resolving actions, which only
// need to know whether there is a history to record an acknowledgement
// against. Folding them back into recordedSignals would cost each of them a
// second read of the same table.
func (s *Server) currentRound(ctx context.Context) (*kitchenv1alpha1.SignalEvaluationStatus, bool) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return nil, false
	}
	status := kitchen.Status.Signals
	if status == nil || status.LastEvaluated == nil {
		return nil, false
	}
	interval := kitchen.Spec.Observability.Signals.Interval()
	if seconds := status.IntervalSeconds; seconds > 0 {
		// The interval the round was actually evaluated on, which is the one
		// its age has to be judged against: an operator who has just widened
		// the interval has not made the last round stale.
		interval = time.Duration(seconds) * time.Second
	}
	if time.Since(status.LastEvaluated.Time) > staleRounds*interval {
		return nil, false
	}
	return status, true
}

// signalSources is where a snapshot comes from, on this side of the operator.
func (s *Server) signalSources(ctx context.Context) signals.Sources {
	store := s.signalStore(ctx)
	sources := signals.Sources{
		Client:   cachelessClient{Client: s.Client, reader: s.reader()},
		Store:    store,
		Resolver: s.dnsResolver(),
	}
	sources.HostMetrics = hostMetricsOf(store)
	sources.VolumeUsage = volumeUsageOf(store)
	if s.Flows != nil {
		sources.Ingest = flowIngest{follower: s.Flows}
	}
	return sources
}

// hostMetricsSource and volumeUsageSource are the two screens' way in to the
// same readers the catalogue uses, so a series drawn on the Nodes or Storage
// screen and the rule that fires on it are one reading rather than two that can
// disagree. Nil means the question does not arise; see [hostMetricsOf].
func (s *Server) hostMetricsSource(ctx context.Context) signals.HostMetricsSource {
	return hostMetricsOf(s.signalStore(ctx))
}

func (s *Server) volumeUsageSource(ctx context.Context) signals.VolumeUsageSource {
	return volumeUsageOf(s.signalStore(ctx))
}

// hostMetricsOf and volumeUsageOf adapt a resolved store, and they follow its
// resolution exactly: nil for an installation with no telemetry, and a source
// that fails every read when the store is configured and unreachable.
//
// The distinction is the whole point. Adapting a nil store would produce a
// source that is not nil and cannot answer, which the gatherer reads as
// unreadable — "measured, and we cannot see it" — when the truth is that
// nothing was ever measured.
func hostMetricsOf(store signals.Store) signals.HostMetricsSource {
	reader, ok := store.(signals.NodeUsageReader)
	if !ok {
		return nil
	}
	return signals.StoreHostMetrics(reader)
}

func volumeUsageOf(store signals.Store) signals.VolumeUsageSource {
	reader, ok := store.(signals.VolumeUsageReader)
	if !ok {
		return nil
	}
	return signals.StoreVolumeUsage(reader)
}

// signalStore resolves the telemetry store for an evaluation, and the two ways
// it can be absent are deliberately not the same answer.
//
// An installation that chose to run without telemetry has no store to read, and
// the rules over it do not arise: nil, which the gatherer marks not-applicable
// and reports nothing about. A store that is configured and unreachable is a
// different sentence entirely — those rules cannot see, and saying so is the
// point of this whole package — so it becomes a store that fails every read
// with the reason, which the gatherer marks unreadable and the round reports.
func (s *Server) signalStore(ctx context.Context) signals.Store {
	store, err := s.logStore(ctx)
	switch {
	case errors.Is(err, errNoLogStore):
		return nil
	case err != nil:
		return unreachableStore{err: err}
	default:
		return store
	}
}

// signalPolicy is the installation's thresholds, resolved from the singleton.
//
// Every caller that judges a *recorded* round needs it, because a transition
// carries the tier the rule declared and not what this installation does with
// it — the clock and the paging floor are applied when the row is read, which
// is what makes changing the policy take effect on conditions that are already
// open rather than only on the next thing to break.
//
// A singleton that cannot be read answers with the default rather than an
// error, for the reason [signals.Gather] does the same: the alternative is an
// alerts screen that fails whole because a setting could not be fetched.
func (s *Server) signalPolicy(ctx context.Context) signals.Policy {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return signals.DefaultPolicy()
	}
	return signals.PolicyFrom(kitchen)
}

// dnsResolver is how dns.mismatch resolves a published name. It is the
// catalogue's own bounded resolver rather than a second copy of one, so that
// this evaluation and the background loop's cannot disagree about whether a
// name resolves.
func (s *Server) dnsResolver() signals.Resolver {
	if s.resolver != nil {
		return s.resolver
	}
	return signals.SystemResolver(dnsLookupTimeout)
}

// FlowIngest adapts the follower's loss ledger to what the catalogue asks for.
//
// The two shapes differ because they are answering different questions: the
// follower counts what it saw go missing, and the rule wants to know whether
// the request numbers under-report. The adapter lives on this side of the
// seam so that neither the follower nor the signals package has to know the
// other exists — and it is exported because the catalogue now has a second
// caller: the operator's background evaluation loop gathers the same sources,
// and two copies of this mapping would be two rounds that can disagree about
// how much was lost.
func FlowIngest(follower FlowFollower) signals.IngestAccounting {
	return flowIngest{follower: follower}
}

type flowIngest struct {
	follower FlowFollower
}

func (f flowIngest) IngestHealth(context.Context) (signals.IngestHealth, error) {
	loss := f.follower.Loss(flows.LossWindow)
	return signals.IngestHealth{
		Window:    loss.Window,
		FlowsLost: loss.Events,
		// The ledger counts reconnects as an unsigned tally of a ring of
		// minutes; the rule counts them as "how many gaps", which cannot
		// overflow an int at one per stream drop.
		Reconnects: int(loss.Reconnects),
		LastLoss:   loss.Latest,
	}, nil
}

// cachelessClient reads through the API server and defers everything else to
// the manager's client.
//
// The gatherer takes a client.Client because in a reconciler it is meant to be
// the manager's cached one. The API's rule is the opposite, and for the reason
// Server.APIReader documents: these screens ask about every pod, node and claim
// in the cluster, and a warm informer over all of them is a permanent cost for
// a question only an open dashboard asks. So the reads go to the reader, while
// the embedded client still carries the scheme and the RESTMapper the typed
// lists resolve through. Nothing in a gather writes.
type cachelessClient struct {
	client.Client
	reader client.Reader
}

func (c cachelessClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c cachelessClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

// unreachableStore is a telemetry store that answers every read with the reason
// it could not be opened.
//
// It exists so that a store the platform cannot reach degrades honestly. The
// gatherer marks each input it touches unreadable, the registry turns that into
// findings that say the rule could not be evaluated, and the round's
// `unreadable` list names the failure once. The alternative — passing nil —
// would tell the reader this installation has no telemetry store, which is a
// statement about how it was configured and not about what is broken.
type unreachableStore struct {
	err error
}

func (u unreachableStore) RequestSeries(
	context.Context, clickhouse.RequestSeriesQuery,
) (clickhouse.RequestSeries, error) {
	return clickhouse.RequestSeries{}, u.err
}

func (u unreachableStore) ResourceSeries(
	context.Context, clickhouse.ResourceSeriesQuery,
) (clickhouse.ResourceSeries, error) {
	return clickhouse.ResourceSeries{}, u.err
}

func (u unreachableStore) QueryAuditRecords(
	context.Context, clickhouse.AuditQuery,
) ([]clickhouse.AuditRecord, error) {
	return nil, u.err
}

func (u unreachableStore) ProjectTraffic(
	context.Context, clickhouse.ProjectTrafficQuery,
) ([]clickhouse.ProjectTraffic, error) {
	return nil, u.err
}

func (u unreachableStore) UnroutedHosts(
	context.Context, clickhouse.PlatformRequestsQuery,
) ([]clickhouse.UnroutedHost, error) {
	return nil, u.err
}

func (u unreachableStore) QueryK8sEvents(
	context.Context, clickhouse.K8sEventQuery,
) ([]clickhouse.K8sEvent, error) {
	return nil, u.err
}

func (u unreachableStore) TelemetryFreshness(
	context.Context, time.Duration,
) ([]clickhouse.NodeFreshness, error) {
	return nil, u.err
}

func (u unreachableStore) StoreStats(context.Context) (clickhouse.StoreStats, error) {
	return clickhouse.StoreStats{}, u.err
}

// The two optional sources fail the same way, which is the point of spelling
// them out here: a store nobody can reach must make node saturation and volume
// fill unreadable, not absent. Absent is what an installation without telemetry
// looks like, and the difference is the difference between "not measured" and
// "measured, and we cannot see it".

func (u unreachableStore) NodeUsage(
	context.Context, clickhouse.NodeUsageQuery,
) ([]clickhouse.NodeUsage, error) {
	return nil, u.err
}

func (u unreachableStore) VolumeUsage(
	context.Context, clickhouse.VolumeUsageQuery,
) ([]clickhouse.VolumeUsage, error) {
	return nil, u.err
}

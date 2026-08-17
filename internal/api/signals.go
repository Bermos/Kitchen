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
	"net"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/flows"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The problems list and the diagnostics strip: one catalogue, one snapshot,
// narrowed differently. The operator asks what is wrong with the platform; the
// developer asks what is wrong with their environment, and gets the subset of
// the same round that is about it.
//
// Nothing is evaluated on a timer. A screen asks, the gatherer reads the API
// server and the store once, and the thirty-six rules run over the value it
// produced — which is what makes them free to be pure, and what makes this
// endpoint the same shape as the inbox that will one day read
// `signal_transitions` instead.
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
// exist — see boundedResolver.
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
// This screen is the inbox docs/OBSERVABILITY.md §7 designs, minus persistence.
// When background evaluation lands it reads `signal_transitions` instead of
// evaluating here, and answers in this same shape.
func (s *Server) platformSignals(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

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
	// EvaluatedAt is the instant the snapshot was taken. Findings are
	// ephemeral today, so this is how a screen says how fresh they are.
	EvaluatedAt time.Time `json:"evaluatedAt"`

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
	body := signalsBody{
		Items:       itemsOf(findings),
		Unreadable:  snapshot.Unreadable(),
		EvaluatedAt: snapshot.Now,
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

// dnsResolver is how dns.mismatch resolves a published name.
func (s *Server) dnsResolver() signals.Resolver {
	if s.resolver != nil {
		return s.resolver
	}
	return boundedResolver{resolver: net.DefaultResolver, timeout: dnsLookupTimeout}
}

// boundedResolver is the standard library's resolver with a deadline per
// lookup.
//
// The deadline is what makes this safe to call from a request handler, and it
// is also why the type exists rather than passing net.DefaultResolver
// straight through. The distinction the rule rests on survives it: the resolver
// answers a lookup that timed out with a *net.DNSError whose IsNotFound is
// false, which the gatherer reads as an input it could not read — and even an
// error that carried no DNSError at all would fail that test the same way.
// Only a name the resolver positively says does not exist becomes a finding.
type boundedResolver struct {
	resolver *net.Resolver
	timeout  time.Duration
}

func (r boundedResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.resolver.LookupHost(ctx, host)
}

// flowIngest adapts the follower's loss ledger to what the catalogue asks for.
//
// The two shapes differ because they are answering different questions: the
// follower counts what it saw go missing, and the rule wants to know whether
// the request numbers under-report. The adapter lives here — in the caller,
// where the two packages are already both in scope — so that neither the
// follower nor the signals package has to know the other exists.
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

func (u unreachableStore) MetricsOverview(
	context.Context, clickhouse.MetricsQuery,
) (clickhouse.MetricsOverview, error) {
	return clickhouse.MetricsOverview{}, u.err
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
